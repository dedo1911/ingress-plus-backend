package routes

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/dedo1911/ingress-plus-backend/internal/players"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

type Media struct {
	InInventory struct {
		// PlayerID is read from the upload and hashed, then cleared before the
		// payload is stored - see stripPlayerID. omitempty keeps the key out of
		// original_data entirely rather than leaving a telltale empty string.
		PlayerID               string `json:"playerId,omitempty"`
		AcquisitionTimestampMs string `json:"acquisitionTimestampMs"`
	} `json:"inInventory"`
	ResourceWithLevels struct {
		ResourceType string `json:"resourceType"`
		Level        int    `json:"level"`
	} `json:"resourceWithLevels"`
	ImageByURL struct {
		ImageURL string `json:"imageUrl"`
	} `json:"imageByUrl"`
	DisplayName struct {
		DisplayName string `json:"displayName"`
	} `json:"displayName"`
	StoryItem struct {
		PrimaryURL       string `json:"primaryUrl"`
		ShortDescription string `json:"shortDescription"`
		MediaID          string `json:"mediaId"`
		HasBeenViewed    bool   `json:"hasBeenViewed"`
		ReleaseDate      string `json:"releaseDate"`
	} `json:"storyItem"`
}

type Player struct {
	Team                 string `json:"team"`
	Nickname             string `json:"nickname"`
	Ap                   string `json:"ap"`
	Energy               int    `json:"energy"`
	AvailableInvites     int    `json:"available_invites"`
	VerifiedLevel        int    `json:"verified_level"`
	XmCapacity           string `json:"xm_capacity"`
	MinApForCurrentLevel string `json:"min_ap_for_current_level"`
	MinApForNextLevel    string `json:"min_ap_for_next_level"`
	Level                int    `json:"level"`
	NickMatcher          struct {
	} `json:"nickMatcher"`
}

type UploadMediaRequest struct {
	Player Player  `json:"player"`
	Medias []Media `json:"medias"`
}

// UploadMedia handles an upload from the Mediagress IITC plugin.
//
// Two collections are written, and the difference between them matters:
//   - "medias" holds one record per Media that exists in the game, created by
//     whoever uploaded it first.
//   - "media_uploads" logs every agent who has ever uploaded each Media, and is
//     what the site's contribution stats and "my uploads" are built from.
//
// Every upload therefore produces a media_uploads row whether or not the Media
// itself was already known. Until this change the handler skipped already-known
// Media outright, so repeat uploads went unrecorded and firstTimeUserUploadCount
// could never differ from previouslyUnknownMediaCount - the "already on
// Mediagress but new to you" case it was added for was unreachable.
//
// The route is unauthenticated: the plugin runs on intel.ingress.com with no
// Ingress Plus session, so nickname and faction are self-asserted. Treat them
// as display data - the hashed player ID is the identity, and only the
// verification flow makes it trustworthy.
func UploadMedia(hasher *players.Hasher) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		defer e.Request.Body.Close()
		body, err := io.ReadAll(e.Request.Body)
		if err != nil {
			return newErrorResponse(e, err, http.StatusInternalServerError, "Failed to read request body")
		}
		var data UploadMediaRequest
		if err := json.Unmarshal(body, &data); err != nil {
			return newErrorResponse(e, err, http.StatusBadRequest, "Invalid JSON format")
		}

		e.App.Logger().DebugContext(e.Request.Context(), "Received upload media request", slog.String("player", data.Player.Nickname))

		playerRecordID := resolvePlayer(e, hasher, data)

		newMedias := 0
		firstTimeUserUploads := 0
		var newMediaTitles []string

		for _, media := range data.Medias {
			urlID, created, err := ensureMedia(e.App, media, playerRecordID, data.Player)
			if err != nil {
				return newErrorResponse(e, err, http.StatusInternalServerError, "Error saving media record")
			}
			if created {
				newMedias++
				newMediaTitles = append(newMediaTitles, media.StoryItem.ShortDescription)
			}

			firstTime, err := ensureUpload(e.App, urlID, playerRecordID, data.Player)
			if err != nil {
				return newErrorResponse(e, err, http.StatusInternalServerError, "Error saving media upload record")
			}
			if firstTime {
				firstTimeUserUploads++
			}
		}

		return e.JSON(http.StatusOK, map[string]any{
			"previouslyUnknownMediaCount": newMedias,
			"newMediaTitles":              newMediaTitles,
			"firstTimeUserUploadCount":    firstTimeUserUploads,
		})
	}
}

// resolvePlayer hashes the uploading agent's player ID and returns the id of
// their "players" record, or "" if it could not be determined.
//
// The ID is taken from the first item that carries one: every item in a request
// comes from the same agent's inventory, and no upload session in the
// production data has ever contained more than one distinct player ID.
//
// A missing or unrecognized ID is logged and skipped rather than failing the
// upload. Media is still worth collecting without attribution, and this way a
// change to Niantic's ID format degrades to unattributed uploads instead of a
// hard outage.
func resolvePlayer(e *core.RequestEvent, hasher *players.Hasher, data UploadMediaRequest) string {
	for _, media := range data.Medias {
		raw := media.InInventory.PlayerID
		if raw == "" {
			continue
		}

		hash, err := hasher.Hash(raw)
		if err != nil {
			e.App.Logger().WarnContext(e.Request.Context(), "Unrecognized player ID in upload, continuing without attribution",
				slog.String("player", data.Player.Nickname), slog.Any("error", err))
			return ""
		}

		record, err := players.Ensure(e.App, hash, data.Player.Nickname, data.Player.Team)
		if err != nil {
			e.App.Logger().ErrorContext(e.Request.Context(), "Failed to resolve player record, continuing without attribution",
				slog.String("player", data.Player.Nickname), slog.Any("error", err))
			return ""
		}
		return record.Id
	}

	return ""
}

// stripPlayerID clears the raw player ID so it never reaches original_data.
// Takes a copy: the caller's Media is left untouched.
func stripPlayerID(media Media) Media {
	media.InInventory.PlayerID = ""
	return media
}

// ensureMedia returns the url_id of the "medias" record for this Media,
// creating it if this is the first time anyone has uploaded it. The bool
// reports whether it was created.
func ensureMedia(app core.App, media Media, playerRecordID string, player Player) (int, bool, error) {
	existing, err := app.FindFirstRecordByData("medias", "media_id", media.StoryItem.MediaID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	if existing != nil {
		return existing.GetInt("url_id"), false, nil
	}

	ms, err := strconv.ParseInt(media.StoryItem.ReleaseDate, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parsing release date %q: %w", media.StoryItem.ReleaseDate, err)
	}
	releaseDate := time.UnixMilli(ms)

	rawMedia, err := json.Marshal(stripPlayerID(media))
	if err != nil {
		return 0, false, err
	}

	collection, err := app.FindCollectionByNameOrId("medias")
	if err != nil {
		return 0, false, err
	}

	var urlID int
	// The url_id allocation and the insert share a transaction so two uploads
	// discovering the same new Media at once cannot both read the same MAX and
	// mint the same public id.
	err = app.RunInTransaction(func(txApp core.App) error {
		var maxURLID struct {
			Max int `db:"max"`
		}
		// COALESCE so an empty table yields 0 rather than a NULL that won't scan.
		if err := txApp.DB().NewQuery("SELECT COALESCE(MAX(url_id), 0) max FROM medias").One(&maxURLID); err != nil {
			return err
		}
		urlID = maxURLID.Max + 1

		record := core.NewRecord(collection)
		record.Set("url_id", urlID)
		record.Set("media_id", media.StoryItem.MediaID)
		record.Set("image_url", media.ImageByURL.ImageURL)
		record.Set("content_url", media.StoryItem.PrimaryURL)
		record.Set("short_description", media.StoryItem.ShortDescription)
		record.Set("description", "")
		record.Set("released_at", releaseDate)
		record.Set("uploader_ign", player.Nickname)
		record.Set("uploader_faction", player.Team)
		record.Set("original_data", rawMedia)
		record.Set("level", media.ResourceWithLevels.Level)
		record.Set("approved", true)
		if playerRecordID != "" {
			record.Set("player", playerRecordID)
		}

		return txApp.Save(record)
	})
	if err != nil {
		return 0, false, err
	}

	return urlID, true, nil
}

// ensureUpload records that this agent has uploaded this Media, returning
// whether it was their first time. media_uploads is uniquely indexed on
// (media_url_id, uploader_ign), so this is a find-or-create.
//
// An existing row that predates player attribution is linked opportunistically,
// which is how history gets filled in for agents who keep uploading.
func ensureUpload(app core.App, urlID int, playerRecordID string, player Player) (bool, error) {
	mediaURLID := strconv.Itoa(urlID)

	existing, err := app.FindFirstRecordByFilter(
		"media_uploads",
		"media_url_id = {:media_url_id} && uploader_ign = {:uploader_ign}",
		dbx.Params{"media_url_id": mediaURLID, "uploader_ign": player.Nickname},
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	if existing != nil {
		if playerRecordID != "" && existing.GetString("player") == "" {
			existing.Set("player", playerRecordID)
			if err := app.Save(existing); err != nil {
				return false, err
			}
		}
		return false, nil
	}

	collection, err := app.FindCollectionByNameOrId("media_uploads")
	if err != nil {
		return false, err
	}

	record := core.NewRecord(collection)
	record.Set("uploader_ign", player.Nickname)
	record.Set("uploader_faction", player.Team)
	record.Set("media_url_id", mediaURLID)
	if playerRecordID != "" {
		record.Set("player", playerRecordID)
	}

	if err := app.Save(record); err != nil {
		// Lost the race on the unique index - the row we wanted now exists, so
		// this is not this agent's first upload of it.
		if _, findErr := app.FindFirstRecordByFilter(
			"media_uploads",
			"media_url_id = {:media_url_id} && uploader_ign = {:uploader_ign}",
			dbx.Params{"media_url_id": mediaURLID, "uploader_ign": player.Nickname},
		); findErr == nil {
			return false, nil
		}
		return false, err
	}

	return true, nil
}
