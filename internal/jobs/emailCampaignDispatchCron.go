package jobs

import (
	"log"

	"github.com/dedo1911/ingress-plus-backend/internal/campaigns"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
)

// EmailCampaignDispatchCron picks up any campaign left in "queued" status
// (the admin panel only leaves a campaign queued when its audience was too
// large to send instantly) and dispatches it via the same logic the
// instant-send route uses.
func EmailCampaignDispatchCron(app *pocketbase.PocketBase) func() {
	return func() {
		queued, err := app.FindRecordsByFilter("email_campaigns", "status = {:status}", "created", 0, 0, dbx.Params{"status": "queued"})
		if err != nil {
			log.Println("Failed to query queued email campaigns", err)
			app.Logger().Error("Failed to query queued email campaigns", "error", err)
			return
		}

		for _, campaign := range queued {
			if err := campaigns.DispatchCampaign(app, campaign); err != nil {
				log.Println("Failed to dispatch campaign", campaign.Id, err)
				app.Logger().Error("Failed to dispatch campaign", "id", campaign.Id, "error", err)
			}
		}
	}
}
