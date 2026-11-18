package routes

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/dedo1911/ingress-plus-backend/internal/iitc"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

const iitcMaxBodyBytes = 4 << 10

// RegisterIITC wires the plugin store routes. /api/plugins serves the store
// pages on ingress.plus (PocketBase user auth); /api/iitc/* serves the
// sideloader on intel.ingress.com, which authenticates with the long-lived
// token minted by POST /api/iitc/tokens.
func RegisterIITC(se *core.ServeEvent, watermarker *iitc.Watermarker) {
	se.Router.GET("/api/plugins", StoreListPlugins).Bind(apis.RequireAuth("users"))
	se.Router.GET("/api/plugins/{slug}", StorePluginDetail).Bind(apis.RequireAuth("users"))
	se.Router.POST("/api/iitc/tokens", CreateIITCToken).Bind(apis.RequireAuth("users"))

	group := se.Router.Group("/api/iitc")
	group.Bind(iitc.RequireToken())
	group.GET("/me", IITCMe)
	group.DELETE("/tokens/current", RevokeCurrentIITCToken)
	group.GET("/plugins", IITCListPlugins)
	group.POST("/plugins/{slug}/load", IITCLoadPlugin(watermarker))
}

func notFound(e *core.RequestEvent) error {
	return e.JSON(http.StatusNotFound, map[string]any{"error": "Plugin not found."})
}

func readSmallJSON(e *core.RequestEvent, dst any) error {
	body, err := io.ReadAll(io.LimitReader(e.Request.Body, iitcMaxBodyBytes+1))
	if err != nil || len(body) > iitcMaxBodyBytes {
		return errors.New("request body too large or unreadable")
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, dst)
}

// CreateIITCToken is called by /iitc/connect, signed in on ingress.plus, to
// mint the token the sideloader keeps. The plaintext is returned once.
func CreateIITCToken(e *core.RequestEvent) error {
	if e.Auth == nil || e.Auth.Collection().Name != "users" {
		return newErrorResponse(e, errors.New("not a users auth"), http.StatusForbidden, "Sign in with your Ingress Plus account.")
	}

	var body struct {
		Label string `json:"label"`
	}
	if err := readSmallJSON(e, &body); err != nil {
		return newErrorResponse(e, err, http.StatusBadRequest, "Invalid request body.")
	}

	token, record, err := iitc.CreateToken(e.App, e.Auth, body.Label)
	if errors.Is(err, iitc.ErrTooManyTokens) {
		return newErrorResponse(e, err, http.StatusConflict, err.Error())
	}
	if err != nil {
		return newErrorResponse(e, err, http.StatusInternalServerError, "Could not create the IITC token.")
	}

	e.Response.Header().Set("Cache-Control", "no-store")
	return e.JSON(http.StatusOK, map[string]any{
		"id":    record.Id,
		"label": record.GetString("label"),
		"token": token,
	})
}

// RevokeCurrentIITCToken is the sideloader's Disconnect: it deletes the
// token it authenticated with, so the token stops working wherever it was
// copied to, not just in this browser.
func RevokeCurrentIITCToken(e *core.RequestEvent) error {
	if err := e.App.Delete(iitc.CurrentToken(e)); err != nil {
		return newErrorResponse(e, err, http.StatusInternalServerError, "Could not revoke the token.")
	}
	return e.NoContent(http.StatusNoContent)
}

// IITCMe tells the sideloader whose token it holds.
func IITCMe(e *core.RequestEvent) error {
	user := iitc.TokenUser(e)
	return e.JSON(http.StatusOK, map[string]any{
		"username": user.GetString("username"),
		"faction":  user.GetString("faction"),
		"verified": iitc.IsVerified(user),
	})
}

// listPlugins is shared by the store page and the sideloader: both see
// exactly the plugins the user can access and nothing else.
func listPlugins(e *core.RequestEvent, user *core.Record) error {
	plugins, err := iitc.ListAccessible(e.App, user)
	if err != nil {
		return newErrorResponse(e, err, http.StatusInternalServerError, "Could not list plugins.")
	}
	e.Response.Header().Set("Cache-Control", "private, no-store")
	return e.JSON(http.StatusOK, map[string]any{"plugins": plugins})
}

// IITCListPlugins is the sideloader catalog.
func IITCListPlugins(e *core.RequestEvent) error {
	return listPlugins(e, iitc.TokenUser(e))
}

// StoreListPlugins is the store page catalog (PocketBase user auth).
func StoreListPlugins(e *core.RequestEvent) error {
	return listPlugins(e, e.Auth)
}

// StorePluginDetail is the store detail page. A plugin the user can't access
// is a 404, the same as one that doesn't exist.
func StorePluginDetail(e *core.RequestEvent) error {
	plugin, err := iitc.FindAccessible(e.App, e.Auth, e.Request.PathValue("slug"))
	if err != nil {
		return notFound(e)
	}
	detail, err := iitc.Detail(e.App, plugin, e.Auth)
	if err != nil {
		return newErrorResponse(e, err, http.StatusInternalServerError, "Could not load the plugin.")
	}
	e.Response.Header().Set("Cache-Control", "private, no-store")
	return e.JSON(http.StatusOK, detail)
}

// IITCLoadPlugin serves one plugin's code to the sideloader, watermarked for
// the requesting agent and encrypted to the key pair the sideloader just
// generated (see iitc.Seal for what that does and doesn't protect against).
func IITCLoadPlugin(watermarker *iitc.Watermarker) func(e *core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		user := iitc.TokenUser(e)

		var body struct {
			ClientPub string `json:"clientPub"`
			Version   string `json:"version"`
			// Encodings lists what the sideloader can inflate. Older
			// sideloaders don't send it and get plain code.
			Encodings []string `json:"encodings"`
		}
		if err := readSmallJSON(e, &body); err != nil {
			return newErrorResponse(e, err, http.StatusBadRequest, "Invalid request body.")
		}
		clientPub, err := base64.StdEncoding.DecodeString(body.ClientPub)
		if err != nil || len(clientPub) == 0 {
			return newErrorResponse(e, errors.New("bad clientPub"), http.StatusBadRequest, "clientPub must be a base64 raw P-256 public key.")
		}

		plugin, err := iitc.FindAccessible(e.App, user, e.Request.PathValue("slug"))
		if err != nil {
			return notFound(e)
		}
		version, err := iitc.ResolveVersion(e.App, plugin, user, body.Version)
		if err != nil {
			return notFound(e)
		}

		code, err := iitc.ServedCode(e.App, version)
		if err != nil {
			return newErrorResponse(e, err, http.StatusInternalServerError, "Could not read the plugin file.")
		}

		payload := watermarker.Apply(code, plugin.GetString("slug"), version.GetString("version"), user.Id, time.Now())
		encoding := "identity"
		if slices.Contains(body.Encodings, "gzip") {
			if payload, err = iitc.Gzip(payload); err != nil {
				return newErrorResponse(e, err, http.StatusInternalServerError, "Could not compress the plugin.")
			}
			encoding = "gzip"
		}

		sealed, err := iitc.Seal(clientPub, payload)
		if errors.Is(err, iitc.ErrBadClientKey) {
			return newErrorResponse(e, err, http.StatusBadRequest, err.Error())
		}
		if err != nil {
			return newErrorResponse(e, err, http.StatusInternalServerError, "Could not encrypt the plugin.")
		}

		e.Response.Header().Set("Cache-Control", "no-store")
		return e.JSON(http.StatusOK, map[string]any{
			"slug":       plugin.GetString("slug"),
			"encoding":   encoding,
			"version":    version.GetString("version"),
			"dev":        version.Id != plugin.GetString("current_version"),
			"serverPub":  base64.StdEncoding.EncodeToString(sealed.ServerPub),
			"iv":         base64.StdEncoding.EncodeToString(sealed.IV),
			"ciphertext": base64.StdEncoding.EncodeToString(sealed.Ciphertext),
		})
	}
}
