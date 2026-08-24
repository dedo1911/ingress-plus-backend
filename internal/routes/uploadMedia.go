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

	"github.com/dedo1911/ingress-plus-backend/internal/notify"
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

// uploadMediaOptions carries the deliberate behaviour differences between
// the versions of the endpoint. Only v2 still uploads; v1 is retired (see
// UploadMediaV1) and the option kept for it is the one thing that would have
// to come back if that were ever reversed.
//
// Note that per-agent upload tracking is *not* one of the differences. It
// used to be - v2 recorded a media_uploads row only for media it had just
// created - but that was a defect rather than a decision: media_uploads is
// the log of every agent's upload of every Media, so skipping the
// already-known ones silently dropped most contributions. See uploadMedia.
type uploadMediaOptions struct {
	// approveNewMedia is the "approved" value for freshly created media
	// records: v1 queued them for manual review, v2 publishes them
	// straight away.
	approveNewMedia bool

	// legacyResponse returns the single-field v1 response body instead of
	// the richer v2 one, since old clients parse it strictly.
	legacyResponse bool

	telegram *notify.Telegram
	hasher   *players.Hasher
}

// v1RetiredMessage is shown to agents still running a plugin that posts to v1.
//
// Every plugin release from 1.0.2 onwards appends the response body to the
// alert it shows ("... Error: 410 Gone: <this text>"), so this is the last
// chance to tell those agents anything at all - after the route is removed
// they get a bare 404. Kept to one plain-text line for that reason: it lands
// inside an alert box, not in a console.
const v1RetiredMessage = "Your Mediagress plugin is out of date and your uploads are no longer being saved. " +
	"Please install the current version from https://ingress.plus/media/upload and upload again."

// UploadMediaV1 handles POST /api/mediagress/v1/upload-media, which is
// retired. It answers 410 Gone with an actionable message rather than simply
// disappearing, so agents on an old plugin are told why their uploads stopped
// working and what to do about it.
//
// This is the grace period, not the end state: once the message has been live
// long enough for the userscript's auto-update to have reached everyone, drop
// the route registration in main.go and let it 404.
func UploadMediaV1() func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		e.App.Logger().InfoContext(e.Request.Context(), "Rejected an upload from a retired plugin version",
			slog.String("endpoint", "v1"), slog.String("userAgent", e.Request.UserAgent()))

		return e.String(http.StatusGone, v1RetiredMessage)
	}
}

// UploadMediaV2 handles POST /api/mediagress/v2/upload-media.
func UploadMediaV2(telegram *notify.Telegram, hasher *players.Hasher) func(*core.RequestEvent) error {
	return uploadMedia(uploadMediaOptions{
		approveNewMedia: true,
		legacyResponse:  false,
		telegram:        telegram,
		hasher:          hasher,
	})
}

// uploadMedia handles an upload from the Mediagress IITC plugin.
//
// Two collections are written, and the difference between them matters:
//   - "medias" holds one record per Media that exists in the game, created by
//     whoever uploaded it first.
//   - "media_uploads" logs every agent who has ever uploaded each Media, and is
//     what the site's contribution stats and "my uploads" are built from.
//
// Every upload therefore produces a media_uploads row whether or not the Media
// itself was already known. v2 used to skip already-known media outright, so
// repeat uploads went unrecorded and firstTimeUserUploadCount could never
// differ from previouslyUnknownMediaCount - the "already on Mediagress but new
// to you" case it was added for was unreachable, because the count it tested
// was keyed on a url_id minted one line earlier.
//
// The route is unauthenticated: the plugin runs on intel.ingress.com with no
// Ingress Plus session, so nickname and faction are self-asserted. Treat them
// as display data - the hashed player ID is the identity, and only the
// verification flow makes it trustworthy.
func uploadMedia(opts uploadMediaOptions) func(*core.RequestEvent) error {
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

		playerRecordID := resolvePlayer(e, opts.hasher, data)

		newMedias := 0
		firstTimeUserUploads := 0
		var newMediaTitles []string

		for _, media := range data.Medias {
			urlID, created, err := ensureMedia(e.App, media, playerRecordID, data.Player, opts.approveNewMedia)
			if err != nil {
				return newErrorResponse(e, err, http.StatusInternalServerError, "Error saving media record")
			}
			if created {
				newMedias++
				newMediaTitles = append(newMediaTitles, media.StoryItem.ShortDescription)

				opts.telegram.SendAsync(
					e.App.Logger(),
					opts.telegram.Topics.Media,
					notify.MediaMessage(media.StoryItem.ShortDescription, media.StoryItem.MediaID, data.Player.Nickname),
				)
			}

			firstTime, err := ensureUpload(e.App, urlID, playerRecordID, data.Player)
			if err != nil {
				return newErrorResponse(e, err, http.StatusInternalServerError, "Error saving media upload record")
			}
			if firstTime {
				firstTimeUserUploads++
			}
		}

		if opts.legacyResponse {
			return e.JSON(http.StatusOK, map[string]any{
				"previouslyUnknownMediaCount": newMedias,
			})
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
func ensureMedia(app core.App, media Media, playerRecordID string, player Player, approved bool) (int, bool, error) {
	existing, err := app.FindFirstRecordByData("medias", "media_id", media.StoryItem.MediaID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	if existing != nil {
		return existing.GetInt("url_id"), false, nil
	}

	return createMedia(app, media, playerRecordID, player, approved)
}

// createMedia inserts a newly discovered Media and returns its freshly minted
// url_id, or resolves to the existing record if another upload created it first.
//
// Split out from ensureMedia's existence check on purpose: that check runs
// outside the transaction, so two uploads of the same new Media can both pass
// it. The unique index on media_id is what actually enforces uniqueness - this
// exists to turn its rejection into the right answer rather than a 500, and to
// make that recovery testable without having to win a race.
func createMedia(app core.App, media Media, playerRecordID string, player Player, approved bool) (int, bool, error) {
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
		record.Set("approved", approved)
		if playerRecordID != "" {
			record.Set("player", playerRecordID)
		}

		return txApp.Save(record)
	})
	if err != nil {
		// Rejected by a unique index because another upload created this Media
		// first. Not an error for this request - the Media exists, it just
		// wasn't this agent who discovered it - so re-read and carry on to log
		// their upload.
		if existing, findErr := app.FindFirstRecordByData("medias", "media_id", media.StoryItem.MediaID); findErr == nil && existing != nil {
			return existing.GetInt("url_id"), false, nil
		}
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
