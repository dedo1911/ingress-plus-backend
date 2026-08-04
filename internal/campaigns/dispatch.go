package campaigns

import (
	"fmt"
	"net/mail"
	"strings"
	"time"

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

func recipientPlaceholderValues(recipient *core.Record) map[string]string {
	return map[string]string{
		"username":  recipient.GetString("username"),
		"faction":   recipient.GetString("faction"),
		"userEmail": recipient.GetString("email"),
	}
}

// DispatchCampaign resolves a campaign's audience and sends a personalized
// email to each recipient. Placeholders are substituted per-recipient, so
// unlike an identical-content blast this can't be Bcc-batched - each
// recipient gets their own Send() call. Updates the campaign's
// status/recipientCount/sentAt/error as it goes.
func DispatchCampaign(app core.App, campaign *core.Record) error {
	campaign.Set("status", "sending")
	if err := app.Save(campaign); err != nil {
		return fmt.Errorf("failed to mark campaign as sending: %w", err)
	}

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
