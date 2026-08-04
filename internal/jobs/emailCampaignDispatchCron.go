package jobs

import (
	"database/sql"
	"errors"
	"log"

	"github.com/dedo1911/ingress-plus-backend/internal/campaigns"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
)

// EmailCampaignDispatchCron dispatches queued campaigns one at a time,
// re-querying for "the next queued campaign" on every iteration rather
// than snapshotting the whole list up front. That matters because
// PocketBase's cron scheduler can run overlapping ticks of the same job
// (see ClaimCampaign) - if this instead looped over a list fetched once at
// the start, a campaign further down that list could still show as
// "queued" for the entire time an earlier, slow campaign is being sent,
// and get picked up (and sent) by both this run and a newer overlapping
// one. Re-querying + atomically claiming each campaign closes that gap.
func EmailCampaignDispatchCron(app *pocketbase.PocketBase) func() {
	return func() {
		for {
			campaign, err := app.FindFirstRecordByFilter("email_campaigns", "status = {:status}", dbx.Params{"status": "queued"})
			if err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					log.Println("Failed to query queued email campaigns", err)
					app.Logger().Error("Failed to query queued email campaigns", "error", err)
				}
				return
			}

			claimed, err := campaigns.ClaimCampaign(app, campaign.Id)
			if err != nil {
				log.Println("Failed to claim campaign", campaign.Id, err)
				app.Logger().Error("Failed to claim campaign", "id", campaign.Id, "error", err)
				continue
			}
			if !claimed {
				// Another (likely overlapping) tick claimed it first between
				// our query and this point - don't send it a second time.
				continue
			}

			if err := campaigns.DispatchCampaign(app, campaign); err != nil {
				log.Println("Failed to dispatch campaign", campaign.Id, err)
				app.Logger().Error("Failed to dispatch campaign", "id", campaign.Id, "error", err)
			}
		}
	}
}
