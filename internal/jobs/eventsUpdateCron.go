package jobs

import (
	"log"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/reugn/go-quartz/quartz"
)

func getNextOccurrence(cronExpr string) (time.Time, error) {
	cronTrigger, err := quartz.NewCronTrigger(cronExpr)
	if err != nil {
		return time.Now(), err
	}
	nextExec, err := cronTrigger.NextFireTime(time.Now().UTC().UnixNano())
	return time.Unix(0, nextExec), err
}

func EventsUpdateCron(app *pocketbase.PocketBase) func() {
	return func() {
		events, err := app.FindAllRecords("game_events", dbx.NewExp("repeat_cron != ''"))
		if err != nil {
			app.Logger().Error("Failed to retrieve game_events", "error", err)
			return
		}
		log.Println("Processing", len(events), "events")
		for _, event := range events {
			cronString := event.GetString("repeat_cron")
			nextOccurrence, err := getNextOccurrence(cronString)
			if err != nil {
				log.Println("Failed to get next occurrence for cron", "cron", cronString, "error", err)
				app.Logger().Error("Failed to get next occurrence for cron", "cron", cronString, "error", err)
				continue
			}
			nextOccurrence = nextOccurrence.UTC()
			startTime := event.GetDateTime("start_time").Time()
			endTime := event.GetDateTime("end_time").Time()
			combinedStart := time.Date(
				nextOccurrence.Year(), nextOccurrence.Month(), nextOccurrence.Day(),
				startTime.Hour(), startTime.Minute(), startTime.Second(), startTime.Nanosecond(),
				startTime.Location(),
			)
			combinedEnd := time.Date(
				nextOccurrence.Year(), nextOccurrence.Month(), nextOccurrence.Day(),
				endTime.Hour(), endTime.Minute(), endTime.Second(), endTime.Nanosecond(),
				endTime.Location(),
			)
			if combinedStart.UnixNano()-startTime.UnixNano() == 0 {
				log.Println("Unchanged event", event.Id, cronString, "\t", combinedStart, "->", combinedEnd)
				continue
			}
			log.Println("Updating event", event.Id, cronString, "\t", combinedStart, "->", combinedEnd)
			record, err := app.FindRecordById("game_events", event.Id)
			if err != nil {
				log.Println("Failed to find game_events record", event.Id, ":", err)
				app.Logger().Error("Failed to find game_events record", "id", event.Id, "error", err)
				continue
			}
			record.Set("start_time", combinedStart)
			record.Set("end_time", combinedEnd)
			if err := app.Save(record); err != nil {
				log.Println("Failed to save game_events record", event.Id, ":", err)
				app.Logger().Error("Failed to save game_events record", "id", event.Id, "error", err)
				continue
			}
		}
	}
}
