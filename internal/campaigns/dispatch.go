package campaigns

import (
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/mailer"
)

// RenderPlaceholders substitutes the campaign placeholder tokens
// (%username%, %faction%, %userEmail%) in text using the given values.
func RenderPlaceholders(text string, values map[string]string) string {
	replacer := strings.NewReplacer(
		"%username%", values["username"],
		"%faction%", values["faction"],
		"%userEmail%", values["userEmail"],
	)
	return replacer.Replace(text)
}

// FormatFaction turns PocketBase's raw lowercase faction value into the
// capitalized form used in campaign emails, substituting an in-theme
// placeholder when an agent hasn't set one instead of leaving it blank.
// Exported so the test-send preview route can build its example values the
// same way real recipients' are built below.
func FormatFaction(faction string) string {
	if faction == "" {
		return "UNKNOWN FACTION"
	}
	return strings.ToUpper(faction[:1]) + faction[1:]
}

func recipientPlaceholderValues(recipient *core.Record) map[string]string {
	return map[string]string{
		"username":  recipient.GetString("username"),
		"faction":   FormatFaction(recipient.GetString("faction")),
		"userEmail": recipient.GetString("email"),
	}
}

// ClaimCampaign atomically transitions a campaign from "queued" to
// "sending", returning claimed=false if it was no longer "queued" by the
// time this ran. This matters because PocketBase's cron scheduler fires
// jobs as fire-and-forget goroutines with no protection against overlapping
// runs (see tools/cron.Cron.runDue) - if a previous tick's dispatch is
// still working through a large batch when the next tick fires 5 minutes
// later, both ticks would otherwise see the same not-yet-reached campaign
// as "queued" and send it twice. A plain record.Set+Save read-then-write
// isn't atomic, so this uses a single conditional UPDATE instead.
func ClaimCampaign(app core.App, campaignId string) (bool, error) {
	result, err := app.DB().
		NewQuery("UPDATE email_campaigns SET status = 'sending' WHERE id = {:id} AND status = 'queued'").
		Bind(dbx.Params{"id": campaignId}).
		Execute()
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

// DispatchCampaign resolves a campaign's audience and sends a personalized
// email to each recipient. Placeholders are substituted per-recipient, so
// unlike an identical-content blast this can't be Bcc-batched - each
// recipient gets their own Send() call. Updates the campaign's
// status/recipientCount/sentAt/error as it goes.
//
// The campaign must already be claimed (status "sending") via
// ClaimCampaign before calling this - it doesn't claim it itself, so
// callers can tell "already being handled elsewhere" apart from a real
// failure.
func DispatchCampaign(app core.App, campaign *core.Record) error {
	recipients, err := ResolveAudience(app, campaign.GetString("targeting"))
	if err != nil {
		return failCampaign(app, campaign, err)
	}
	if len(recipients) == 0 {
		return failCampaign(app, campaign, fmt.Errorf("no recipients matched the configured targeting"))
	}

	mailClient := app.NewMailClient()
	fromAddress := mail.Address{
		Name:    app.Settings().Meta.SenderName,
		Address: app.Settings().Meta.SenderAddress,
	}

	sent := 0
	failed := 0
	for _, recipient := range recipients {
		email := recipient.GetString("email")
		if email == "" {
			continue
		}

		values := recipientPlaceholderValues(recipient)
		message := &mailer.Message{
			From:    fromAddress,
			To:      []mail.Address{{Address: email, Name: recipient.GetString("username")}},
			Subject: RenderPlaceholders(campaign.GetString("subject"), values),
			HTML:    RenderPlaceholders(campaign.GetString("body"), values),
		}

		if err := mailClient.Send(message); err != nil {
			app.Logger().Error("Failed to send campaign email", "id", campaign.Id, "recipient", recipient.Id, "error", err)
			failed++
			continue
		}
		sent++
	}

	if sent == 0 {
		return failCampaign(app, campaign, fmt.Errorf("all %d sends failed", failed))
	}

	campaign.Set("status", "sent")
	campaign.Set("recipientCount", sent)
	campaign.Set("sentAt", time.Now())
	if failed > 0 {
		campaign.Set("error", fmt.Sprintf("%d of %d sends failed", failed, sent+failed))
	}
	return app.Save(campaign)
}

func failCampaign(app core.App, campaign *core.Record, sendErr error) error {
	campaign.Set("status", "failed")
	campaign.Set("error", sendErr.Error())
	if err := app.Save(campaign); err != nil {
		return err
	}
	return sendErr
}
