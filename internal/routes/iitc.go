package routes

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	group.GET("/plugins/{slug}", IITCPluginDetail)
	group.POST("/plugins/{slug}/load", IITCLoadPlugin(watermarker))
	group.POST("/boot", IITCBoot(watermarker))
}

func notFound(e *core.RequestEvent) error {
	return e.JSON(http.StatusNotFound, map[string]any{"error": "Plugin not found."})
}

func readSmallJSON(e *core.RequestEvent, dst any) error {
	return readJSON(e, dst, iitcMaxBodyBytes)
}

func readJSON(e *core.RequestEvent, dst any, limit int64) error {
	body, err := io.ReadAll(io.LimitReader(e.Request.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
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

// pluginDetail is shared by the store detail page and the sideloader's
// changelog. A plugin the user can't access is a 404, the same as one that
// doesn't exist.
func pluginDetail(e *core.RequestEvent, user *core.Record) error {
	plugin, err := iitc.FindAccessible(e.App, user, e.Request.PathValue("slug"))
	if err != nil {
		return notFound(e)
	}
	detail, err := iitc.Detail(e.App, plugin, user)
	if err != nil {
		return newErrorResponse(e, err, http.StatusInternalServerError, "Could not load the plugin.")
	}
	e.Response.Header().Set("Cache-Control", "private, no-store")
	return e.JSON(http.StatusOK, detail)
}

// StorePluginDetail is the store detail page (PocketBase user auth).
func StorePluginDetail(e *core.RequestEvent) error {
	return pluginDetail(e, e.Auth)
}

// IITCPluginDetail gives the IITC store the version history, for the
// changelog dialog and the "what's new" popup after an update.
func IITCPluginDetail(e *core.RequestEvent) error {
	return pluginDetail(e, iitc.TokenUser(e))
}

// sealPlugin is the code of one plugin version, watermarked for user and
// encrypted to clientPub, as the sideloader gets it from /load and /boot.
// It returns iitc.ErrNotFound when the plugin doesn't exist or user can't
// access it, and iitc.ErrBadClientKey for a key that isn't a P-256 point.
// dev asks for the author's version still waiting for review; for anyone
// else, or when there's none, it's ignored.
func sealPlugin(app core.App, watermarker *iitc.Watermarker, user *core.Record, slug, requested string, dev bool, clientPub []byte, gzip bool) (map[string]any, error) {
	plugin, err := iitc.FindAccessible(app, user, slug)
	if err != nil {
		return nil, iitc.ErrNotFound
	}
	var version *core.Record
	if dev && requested == "" && iitc.IsAuthor(user, plugin) {
		version = iitc.PendingVersion(app, plugin)
	}
	if version == nil {
		if version, err = iitc.ResolveVersion(app, plugin, user, requested); err != nil {
			return nil, iitc.ErrNotFound
		}
	}

	code, err := iitc.ServedCode(app, version)
	if err != nil {
		return nil, fmt.Errorf("reading the plugin file: %w", err)
	}

	payload := watermarker.Apply(code, plugin.GetString("slug"), version.GetString("version"), user.Id, time.Now())
	encoding := "identity"
	if gzip {
		if payload, err = iitc.Gzip(payload); err != nil {
			return nil, fmt.Errorf("compressing the plugin: %w", err)
		}
		encoding = "gzip"
	}

	sealed, err := iitc.Seal(clientPub, payload)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"slug":       plugin.GetString("slug"),
		"encoding":   encoding,
		"version":    version.GetString("version"),
		"dev":        version.Id != plugin.GetString("current_version"),
		"serverPub":  base64.StdEncoding.EncodeToString(sealed.ServerPub),
		"iv":         base64.StdEncoding.EncodeToString(sealed.IV),
		"ciphertext": base64.StdEncoding.EncodeToString(sealed.Ciphertext),
	}, nil
}

func decodeClientPub(raw string) ([]byte, error) {
	clientPub, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(clientPub) == 0 {
		return nil, errors.New("bad clientPub")
	}
	return clientPub, nil
}

// IITCLoadPlugin serves one plugin's code to the sideloader, watermarked for
// the requesting agent and encrypted to the key pair the sideloader just
// generated (see iitc.Seal for what that does and doesn't protect against).
func IITCLoadPlugin(watermarker *iitc.Watermarker) func(e *core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
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
		clientPub, err := decodeClientPub(body.ClientPub)
		if err != nil {
			return newErrorResponse(e, err, http.StatusBadRequest, "clientPub must be a base64 raw P-256 public key.")
		}

		out, err := sealPlugin(e.App, watermarker, iitc.TokenUser(e), e.Request.PathValue("slug"), body.Version, false,
			clientPub, slices.Contains(body.Encodings, "gzip"))
		switch {
		case errors.Is(err, iitc.ErrNotFound):
			return notFound(e)
		case errors.Is(err, iitc.ErrBadClientKey):
			return newErrorResponse(e, err, http.StatusBadRequest, err.Error())
		case err != nil:
			return newErrorResponse(e, err, http.StatusInternalServerError, "Could not load the plugin.")
		}

		e.Response.Header().Set("Cache-Control", "no-store")
		return e.JSON(http.StatusOK, out)
	}
}

const (
	iitcBootMaxBodyBytes = 16 << 10
	iitcBootMaxPlugins   = 50
)

// IITCBoot is the sideloader's startup call: the catalog and the code of
// every plugin it wants to load, in one round trip instead of the catalog
// first and then one /load per plugin.
//
// loads follows the order of the request. A plugin that doesn't exist or
// that the agent can't access gets {"slug", "error": "Plugin not found."},
// the same answer for both cases, as on /load. The other plugins still
// load.
func IITCBoot(watermarker *iitc.Watermarker) func(e *core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		var body struct {
			ClientPub string   `json:"clientPub"`
			Encodings []string `json:"encodings"`
			Plugins   []struct {
				Slug string `json:"slug"`
				// Dev asks for the author's version waiting for review.
				Dev bool `json:"dev"`
			} `json:"plugins"`
		}
		if err := readJSON(e, &body, iitcBootMaxBodyBytes); err != nil {
			return newErrorResponse(e, err, http.StatusBadRequest, "Invalid request body.")
		}
		if len(body.Plugins) > iitcBootMaxPlugins {
			return newErrorResponse(e, errors.New("too many plugins"), http.StatusBadRequest,
				fmt.Sprintf("Ask for at most %d plugins at once.", iitcBootMaxPlugins))
		}
		clientPub, err := decodeClientPub(body.ClientPub)
		if err != nil && len(body.Plugins) > 0 {
			return newErrorResponse(e, err, http.StatusBadRequest, "clientPub must be a base64 raw P-256 public key.")
		}

		user := iitc.TokenUser(e)
		catalog, err := iitc.ListAccessible(e.App, user)
		if err != nil {
			return newErrorResponse(e, err, http.StatusInternalServerError, "Could not list plugins.")
		}

		gzip := slices.Contains(body.Encodings, "gzip")
		loads := make([]map[string]any, 0, len(body.Plugins))
		seen := map[string]bool{}
		for _, want := range body.Plugins {
			if seen[want.Slug] {
				continue
			}
			seen[want.Slug] = true
			out, err := sealPlugin(e.App, watermarker, user, want.Slug, "", want.Dev, clientPub, gzip)
			switch {
			case errors.Is(err, iitc.ErrNotFound):
				out = map[string]any{"slug": want.Slug, "error": "Plugin not found."}
			case errors.Is(err, iitc.ErrBadClientKey):
				return newErrorResponse(e, err, http.StatusBadRequest, err.Error())
			case err != nil:
				e.App.Logger().Error("iitc boot: loading plugin", "slug", want.Slug, "error", err)
				out = map[string]any{"slug": want.Slug, "error": "Could not load the plugin."}
			}
			loads = append(loads, out)
		}

		e.Response.Header().Set("Cache-Control", "no-store")
		return e.JSON(http.StatusOK, map[string]any{"plugins": catalog, "loads": loads})
	}
}
