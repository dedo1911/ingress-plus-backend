package routes

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"

	"github.com/dedo1911/ingress-plus-backend/internal/campaigns"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/mailer"
)

type sendTestCampaignRequest struct {
	Subject string `json:"subject"`
	HTML    string `json:"html"`
}

// SendTestCampaign sends the given subject/HTML to the requesting
// superuser's own email address instantly, with the per-recipient
// placeholders filled in using example values (the requesting _superusers
// record has no username/faction of its own to substitute with), so they
// can preview a campaign before queueing it for the real audience.
// Route should be registered with apis.RequireSuperuserAuth().
func SendTestCampaign(e *core.RequestEvent) error {
	defer e.Request.Body.Close()
	body, err := io.ReadAll(e.Request.Body)
	if err != nil {
		return newErrorResponse(e, err, http.StatusInternalServerError, "Failed to read request body")
	}
	var data sendTestCampaignRequest
	if err := json.Unmarshal(body, &data); err != nil {
		return newErrorResponse(e, err, http.StatusBadRequest, "Invalid JSON format")
	}

	values := map[string]string{
		"username":  "TestAgent",
		"faction":   "resistance",
		"userEmail": e.Auth.Email(),
	}

	message := &mailer.Message{
		From: mail.Address{
			Name:    e.App.Settings().Meta.SenderName,
			Address: e.App.Settings().Meta.SenderAddress,
		},
		To:      []mail.Address{{Address: e.Auth.Email()}},
		Subject: "[TEST] " + campaigns.RenderPlaceholders(data.Subject, values),
		HTML:    campaigns.RenderPlaceholders(data.HTML, values),
	}

	if err := e.App.NewMailClient().Send(message); err != nil {
		return newErrorResponse(e, err, http.StatusInternalServerError, "Failed to send test email")
	}

	return e.JSON(http.StatusOK, map[string]any{"sentTo": e.Auth.Email()})
}

type previewAudienceRequest struct {
	Targeting json.RawMessage `json:"targeting"`
}

// PreviewAudienceCount resolves how many users the given targeting rules
// currently match, without sending anything - used by the admin panel to
// decide (and show) whether a send will go out instantly or get queued.
// Route should be registered with apis.RequireSuperuserAuth().
func PreviewAudienceCount(e *core.RequestEvent) error {
	defer e.Request.Body.Close()
	body, err := io.ReadAll(e.Request.Body)
	if err != nil {
		return newErrorResponse(e, err, http.StatusInternalServerError, "Failed to read request body")
	}
	var data previewAudienceRequest
	if err := json.Unmarshal(body, &data); err != nil {
		return newErrorResponse(e, err, http.StatusBadRequest, "Invalid JSON format")
	}

	recipients, err := campaigns.ResolveAudience(e.App, string(data.Targeting))
	if err != nil {
		return newErrorResponse(e, err, http.StatusBadRequest, "Invalid targeting")
	}

	return e.JSON(http.StatusOK, map[string]any{"count": len(recipients)})
}

// DispatchCampaignNow immediately processes a single already-created
// campaign (normally status "queued"), rather than waiting for the next
// cron tick. Intended for small audiences the admin panel decides to send
// instantly instead of queueing.
//
// Claims the campaign the same way the cron job does, so a double-click,
// retry, or an unlucky overlap with the cron job can't send it twice - if
// something else already claimed it first, this reports a conflict instead
// of sending again.
// Route should be registered with apis.RequireSuperuserAuth().
func DispatchCampaignNow(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")

	claimed, err := campaigns.ClaimCampaign(e.App, id)
	if err != nil {
		return newErrorResponse(e, err, http.StatusInternalServerError, "Failed to claim campaign")
	}
	if !claimed {
		return newErrorResponse(e, fmt.Errorf("campaign %s is not in queued status", id), http.StatusConflict, "Campaign is already being sent or was already sent")
	}

	campaign, err := e.App.FindRecordById("email_campaigns", id)
	if err != nil {
		return newErrorResponse(e, err, http.StatusNotFound, "Campaign not found")
	}

	if err := campaigns.DispatchCampaign(e.App, campaign); err != nil {
		return newErrorResponse(e, err, http.StatusInternalServerError, "Failed to dispatch campaign")
	}

	return e.JSON(http.StatusOK, map[string]any{
		"status":         campaign.GetString("status"),
		"recipientCount": campaign.GetInt("recipientCount"),
	})
}
