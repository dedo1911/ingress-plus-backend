package jobs

import (
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/reugn/go-quartz/quartz"
)

// localEventSlack is how long after a time_type=local event's stored end the
// cron waits before rolling it forward. Local events store wall-clock time
// as if it were UTC and the site re-reads it in the agent's own zone, so an
// all-day event that "ends" at 23:59:59Z is still running until 11:59Z the
// next day for someone at UTC-12.
const localEventSlack = 12 * time.Hour

// nextOccurrence returns the start and end of the occurrence that follows
// the current one, or ok=false when the current occurrence hasn't finished
// yet and the event must be left alone.
//
// NextFireTime returns the next *future* fire by definition, so recomputing
// it while an event is running used to advance start_time past itself on the
// next hourly tick - a live recurring event never read as "happening now".
// The end is carried forward as the original duration rather than rebuilt
// from the new date plus the old clock, which put the end of a 22:00-02:00
// event twenty hours before its start.
func nextOccurrence(now, start, end time.Time, local bool, cronExpr string) (time.Time, time.Time, bool, error) {
	over := end
	if local {
		over = end.Add(localEventSlack)
	}
	if now.Before(over) {
		return start, end, false, nil
	}

	trigger, err := quartz.NewCronTrigger(cronExpr)
	if err != nil {
		return start, end, false, err
	}
	next, err := trigger.NextFireTime(now.UTC().UnixNano())
	if err != nil {
		return start, end, false, err
	}
	fire := time.Unix(0, next).UTC()

	newStart := time.Date(
		fire.Year(), fire.Month(), fire.Day(),
		start.Hour(), start.Minute(), start.Second(), start.Nanosecond(),
		start.Location(),
	)
	return newStart, newStart.Add(end.Sub(start)), true, nil
}

func EventsUpdateCron(app *pocketbase.PocketBase) func() {
	return func() {
		events, err := app.FindAllRecords("game_events", dbx.NewExp("repeat_cron != ''"))
		if err != nil {
			app.Logger().Error("Failed to retrieve game_events", "error", err)
			return
		}
		now := time.Now().UTC()
		for _, event := range events {
			cronExpr := event.GetString("repeat_cron")
			start, end, advance, err := nextOccurrence(
				now,
				event.GetDateTime("start_time").Time(),
				event.GetDateTime("end_time").Time(),
				event.GetString("time_type") == "local",
				cronExpr,
			)
			if err != nil {
				app.Logger().Error("Failed to get next occurrence for cron", "id", event.Id, "cron", cronExpr, "error", err)
				continue
			}
			if !advance {
				continue
			}
			app.Logger().Info("Advancing recurring event", "id", event.Id, "cron", cronExpr, "start", start, "end", end)
			event.Set("start_time", start)
			event.Set("end_time", end)
			if err := app.Save(event); err != nil {
				app.Logger().Error("Failed to save game_events record", "id", event.Id, "error", err)
			}
		}
	}
}
