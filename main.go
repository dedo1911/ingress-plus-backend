package main

import (
	"log"

	"github.com/dedo1911/ingress-plus-backend/internal/campaigns"
	"github.com/dedo1911/ingress-plus-backend/internal/jobs"
	"github.com/dedo1911/ingress-plus-backend/internal/notify"
	"github.com/dedo1911/ingress-plus-backend/internal/routes"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

func main() {
	app := pocketbase.New()

	telegram := notify.TelegramFromEnv()

	// Register custom routes
	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		// v1 is the legacy endpoint that used to be served by
		// pb_hooks/mediagress.pb.js, kept for clients that were never
		// updated to v2 (see routes.UploadMediaV1 for the differences).
		se.Router.POST("/api/mediagress/v1/upload-media", routes.UploadMediaV1(telegram))
		se.Router.POST("/api/mediagress/v2/upload-media", routes.UploadMediaV2(telegram))
		se.Router.POST("/api/admin/campaigns/send-test", routes.SendTestCampaign).Bind(apis.RequireSuperuserAuth())
		se.Router.POST("/api/admin/campaigns/preview-count", routes.PreviewAudienceCount).Bind(apis.RequireSuperuserAuth())
		se.Router.POST("/api/admin/campaigns/{id}/dispatch", routes.DispatchCampaignNow).Bind(apis.RequireSuperuserAuth())
		return se.Next()
	})

	// Block REST updates that would flip an already-sent/sending campaign
	// back to "queued"/"draft" and cause the cron to re-send it.
	app.OnRecordUpdateRequest("email_campaigns").BindFunc(campaigns.GuardCampaignUpdateRequest)

	// Telegram notifications for badge and bug report changes
	notify.RegisterHooks(app, telegram)

	// Register the cron job for updating events
	app.Cron().MustAdd("eventsUpdateCron", "@hourly", jobs.EventsUpdateCron(app))
	app.Cron().MustAdd("statisticsUpdateCron", "@hourly", jobs.StatisticsUpdateCron(app))
	app.Cron().MustAdd("emailCampaignDispatchCron", "*/5 * * * *", jobs.EmailCampaignDispatchCron(app))

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
