package jobs

import (
	"log"

	"github.com/pocketbase/pocketbase"
)

func StatisticsUpdateCron(app *pocketbase.PocketBase) func() {
	return func() {
		record, err := app.FindRecordById("statistics", "000000000000000")
		if err != nil {
			app.Logger().Error("Failed to retrieve statistics record", "error", err)
			return
		}

		totalBadges, err := app.CountRecords("badges")
		if err != nil {
			log.Println("Failed to count badges", err)
			app.Logger().Error("Failed to count badges", "error", err)
			return
		}
		totalMedia, err := app.CountRecords("medias")
		if err != nil {
			log.Println("Failed to count medias", err)
			app.Logger().Error("Failed to count medias", "error", err)
			return
		}
		totalMediaUploads, err := app.CountRecords("media_uploads")
		if err != nil {
			log.Println("Failed to count media_uploads", err)
			app.Logger().Error("Failed to count media_uploads", "error", err)
			return
		}
		totalOwnedBadges, err := app.CountRecords("user_badges")
		if err != nil {
			log.Println("Failed to count user_badges", err)
			app.Logger().Error("Failed to count user_badges", "error", err)
			return
		}
		totalUsers, err := app.CountRecords("users")
		if err != nil {
			log.Println("Failed to count users", err)
			app.Logger().Error("Failed to count users", "error", err)
			return
		}

		type Stats struct {
			MaxMediaID                 int    `db:"max_media_id"`
			UserLastCreated            string `db:"user_last_created"`
			UniqueMediaContributors    int    `db:"unique_media_contributors"`
			UniqueNewMediaContributors int    `db:"unique_new_media_contributors"`
		}
		stats := new(Stats)
		if err := app.DB().NewQuery(`SELECT
		(SELECT MAX(media_id) FROM medias) AS max_media_id,
		(SELECT MAX(created) FROM users LIMIT 1) AS user_last_created,
		(SELECT COUNT(DISTINCT uploader_ign) FROM media_uploads) AS unique_media_contributors,
		(SELECT COUNT(DISTINCT uploader_ign) FROM medias) AS unique_new_media_contributors`).
			One(&stats); err != nil {
			log.Println("Failed to query stats", err)
			app.Logger().Error("Failed to query stats", "error", err)
			return
		}

		record.Set("total_badges", totalBadges)
		record.Set("total_media", totalMedia)
		record.Set("total_media_uploads", totalMediaUploads)
		record.Set("total_owned_badges", totalOwnedBadges)
		record.Set("total_users", totalUsers)
		record.Set("estimate_total_media", stats.MaxMediaID)
		record.Set("unique_media_contributors", stats.UniqueMediaContributors)
		record.Set("unique_new_media_contributors", stats.UniqueNewMediaContributors)
		record.Set("user_last_created", stats.UserLastCreated)

		if err := app.Save(record); err != nil {
			log.Println("Failed to save statistics record", err)
			app.Logger().Error("Failed to save statistics record", "error", err)
			return
		}
	}
}
