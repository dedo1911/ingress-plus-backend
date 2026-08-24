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
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

type Media struct {
	InInventory struct {
		PlayerID               string `json:"playerId"`
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
// the two versions of the endpoint. v1 is frozen: it has to keep matching
// what pb_hooks/mediagress.pb.js did, because clients too old to be
// updated still call it.
type uploadMediaOptions struct {
	// approveNewMedia is the "approved" value for freshly created media
	// records: v1 queues them for manual review, v2 publishes them
	// straight away.
	approveNewMedia bool

	// trackKnownMediaUploads records an upload attempt for media the
	// database already knows about, so v1 keeps building the per-agent
	// upload history for every media in the payload. v2 only tracks the
	// media it just created.
	trackKnownMediaUploads bool

	// legacyResponse returns the single-field v1 response body instead of
	// the richer v2 one, since old clients parse it strictly.
	legacyResponse bool

	telegram *notify.Telegram
}

// UploadMediaV1 handles POST /api/mediagress/v1/upload-media, the legacy
// endpoint kept alive for clients that were never updated to v2.
func UploadMediaV1(telegram *notify.Telegram) func(*core.RequestEvent) error {
	return uploadMedia(uploadMediaOptions{
		approveNewMedia:        false,
		trackKnownMediaUploads: true,
		legacyResponse:         true,
		telegram:               telegram,
	})
}

// UploadMediaV2 handles POST /api/mediagress/v2/upload-media.
func UploadMediaV2(telegram *notify.Telegram) func(*core.RequestEvent) error {
	return uploadMedia(uploadMediaOptions{
		approveNewMedia:        true,
		trackKnownMediaUploads: false,
		legacyResponse:         false,
		telegram:               telegram,
	})
}

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

		newMedias := 0
		firstTimeUserUploads := 0
		var newMediaTitles []string

		for _, media := range data.Medias {
			record, err := e.App.FindFirstRecordByData("medias", "media_id", media.StoryItem.MediaID)

			if err == nil { // The media is already known
				e.App.Logger().DebugContext(e.Request.Context(), "Existing media", slog.String("player", data.Player.Nickname), slog.String("media_id", media.StoryItem.MediaID))
				if !opts.trackKnownMediaUploads {
					continue
				}
				tracked, err := trackMediaUpload(e, data.Player, record.GetInt("url_id"))
				if err != nil {
					return err
				}
				if tracked {
					firstTimeUserUploads++
				}
				continue
			}

			if !errors.Is(err, sql.ErrNoRows) { // An unexpected error occurred
				return newErrorResponse(e, err, http.StatusInternalServerError, "Error finding media record")
			}

			// Get current max URL ID
			var maxURLID struct {
				Max int `db:"max"`
			}
			if err := e.App.DB().NewQuery("SELECT MAX(url_id) max FROM medias").One(&maxURLID); err != nil {
				return newErrorResponse(e, err, http.StatusInternalServerError, "Error getting max URL ID")
			}
			urlID := maxURLID.Max + 1

			// Create a new media record
			mediaCollection, err := e.App.FindCollectionByNameOrId("medias")
			if err != nil {
				return newErrorResponse(e, err, http.StatusInternalServerError, "Error finding medias collection")
			}

			// Parse ReleaseDate
			ms, err := strconv.ParseInt(media.StoryItem.ReleaseDate, 10, 64)
			if err != nil {
				return newErrorResponse(e, err, http.StatusBadRequest, "Error parsing release date")
			}
			releaseDate := time.UnixMilli(ms)

			rawMedia, err := json.Marshal(media)
			if err != nil {
				return newErrorResponse(e, err, http.StatusInternalServerError, "Error marshalling media data")
			}

			mediaRecord := core.NewRecord(mediaCollection)
			mediaRecord.Set("url_id", urlID)
			mediaRecord.Set("media_id", media.StoryItem.MediaID)
			mediaRecord.Set("image_url", media.ImageByURL.ImageURL)
			mediaRecord.Set("content_url", media.StoryItem.PrimaryURL)
			mediaRecord.Set("short_description", media.StoryItem.ShortDescription)
			mediaRecord.Set("description", "")
			mediaRecord.Set("released_at", releaseDate)
			mediaRecord.Set("uploader_ign", data.Player.Nickname)
			mediaRecord.Set("uploader_faction", data.Player.Team)
			mediaRecord.Set("original_data", rawMedia)
			mediaRecord.Set("level", media.ResourceWithLevels.Level)
			mediaRecord.Set("approved", opts.approveNewMedia)

			if err := e.App.Save(mediaRecord); err != nil {
				return newErrorResponse(e, err, http.StatusInternalServerError, "Error saving media record")
			}
			newMedias++
			newMediaTitles = append(newMediaTitles, media.StoryItem.ShortDescription)

			opts.telegram.SendAsync(
				e.App.Logger(),
				opts.telegram.Topics.Media,
				notify.MediaMessage(media.StoryItem.ShortDescription, media.StoryItem.MediaID, data.Player.Nickname),
			)

			tracked, err := trackMediaUpload(e, data.Player, urlID)
			if err != nil {
				return err
			}
			if tracked {
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

// trackMediaUpload stores this agent's upload attempt for the given media
// unless one is already on record, and reports whether it stored a new one.
// The returned error is an already-written JSON error response.
func trackMediaUpload(e *core.RequestEvent, player Player, urlID int) (bool, error) {
	mediaURLID := fmt.Sprintf("%d", urlID)

	// Check if this user has ever uploaded this media before
	var mediaUploads struct {
		Count int `db:"count"`
	}
	if err := e.App.DB().
		NewQuery("SELECT COUNT(*) count FROM media_uploads WHERE uploader_ign = {:uploader_ign} AND media_url_id = {:media_url_id}").
		Bind(dbx.Params{
			"uploader_ign": player.Nickname,
			"media_url_id": mediaURLID,
		}).
		One(&mediaUploads); err != nil {
		return false, newErrorResponse(e, err, http.StatusInternalServerError, "Error checking media uploads")
	}
	if mediaUploads.Count > 0 {
		return false, nil
	}

	uploadCollection, err := e.App.FindCollectionByNameOrId("media_uploads")
	if err != nil {
		return false, newErrorResponse(e, err, http.StatusInternalServerError, "Error finding media uploads collection")
	}
	uploadRecord := core.NewRecord(uploadCollection)
	uploadRecord.Set("uploader_ign", player.Nickname)
	uploadRecord.Set("uploader_faction", player.Team)
	uploadRecord.Set("media_url_id", mediaURLID)
	if err := e.App.Save(uploadRecord); err != nil {
		return false, newErrorResponse(e, err, http.StatusInternalServerError, "Error saving media upload record")
	}

	return true, nil
}
