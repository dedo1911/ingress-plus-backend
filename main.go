package main

import (
	"log"

	"github.com/dedo1911/ingress-plus-backend/internal/jobs"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/plugins/jsvm"
)

func main() {
	app := pocketbase.New()

	app.Cron().MustAdd("eventsUpdateCron", "@hourly", jobs.EventsUpdateCron(app))

	jsvm.MustRegister(app, jsvm.Config{})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
