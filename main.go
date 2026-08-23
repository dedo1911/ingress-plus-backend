package main

import (
	"log"

	"github.com/dedo1911/ingress-plus-backend/internal/backfill"
	"github.com/dedo1911/ingress-plus-backend/internal/campaigns"
	"github.com/dedo1911/ingress-plus-backend/internal/jobs"
	"github.com/dedo1911/ingress-plus-backend/internal/players"
	"github.com/dedo1911/ingress-plus-backend/internal/routes"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/jsvm"
)

func main() {
	app := pocketbase.New()

	// Fail fast rather than silently writing reversible hashes: without the
	// pepper the stored player identifiers would be a plain digest of an ID
	// that used to be public, and so trivially reversible.
	hasher, err := players.NewHasherFromEnv()
	if err != nil {
		log.Fatal(err)
	}

	// Register custom routes
	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		se.Router.POST("/api/mediagress/v2/upload-media", routes.UploadMedia(hasher))
		se.Router.POST("/api/admin/campaigns/send-test", routes.SendTestCampaign).Bind(apis.RequireSuperuserAuth())
		se.Router.POST("/api/admin/campaigns/preview-count", routes.PreviewAudienceCount).Bind(apis.RequireSuperuserAuth())
		se.Router.POST("/api/admin/campaigns/{id}/dispatch", routes.DispatchCampaignNow).Bind(apis.RequireSuperuserAuth())
		return se.Next()
	})

	// Block REST updates that would flip an already-sent/sending campaign
	// back to "queued"/"draft" and cause the cron to re-send it.
	app.OnRecordUpdateRequest("email_campaigns").BindFunc(campaigns.GuardCampaignUpdateRequest)

	// Register the cron job for updating events
	app.Cron().MustAdd("eventsUpdateCron", "@hourly", jobs.EventsUpdateCron(app))
	app.Cron().MustAdd("statisticsUpdateCron", "@hourly", jobs.StatisticsUpdateCron(app))
	app.Cron().MustAdd("emailCampaignDispatchCron", "*/5 * * * *", jobs.EmailCampaignDispatchCron(app))

	// One-off data migrations, run by hand rather than on boot
	app.RootCmd.AddCommand(backfill.NewCommand(app))

	// Enable JS pb_hooks
	jsvm.MustRegister(app, jsvm.Config{})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
