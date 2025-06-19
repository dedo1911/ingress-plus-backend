package main

import (
	"log"

	"github.com/dedo1911/ingress-plus-backend/internal/jobs"
	"github.com/dedo1911/ingress-plus-backend/internal/routes"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/jsvm"
)

func main() {
	app := pocketbase.New()

	// Register custom routes
	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		se.Router.POST("/api/mediagress/v1/upload-media", routes.UploadMedia)

		return se.Next()
	})

	// Register the cron job for updating events
	app.Cron().MustAdd("eventsUpdateCron", "@hourly", jobs.EventsUpdateCron(app))

	// Enable JS pb_hooks
	jsvm.MustRegister(app, jsvm.Config{})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
