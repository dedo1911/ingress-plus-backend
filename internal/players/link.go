package players

import (
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// linkedCollections are the collections carrying a "player" relation back to
// this one. Both record an uploader_ign, which is the only thing tying their
// historical rows to an agent.
var linkedCollections = []string{"medias", "media_uploads"}

// ClaimResult reports what ClaimHistory did.
type ClaimResult struct {
	// Linked is how many rows were pointed at the player record.
	Linked int

	// Conflict is set when the nickname is already attributed to a different
	// player and nothing was touched. It is not an error - it means a human
	// should look, because one of the two attributions is wrong.
	Conflict bool
}

// ClaimHistory attaches every previously unattributed medias / media_uploads
// row bearing this nickname to the given player record.
//
// An upload proves the pair directly: the nickname and the player ID arrive
// together in one live inventory read, unlike the legacy import the backfill
// has to work around. And because Ingress never releases a username once it has
// been used, that pair holds for every row the name has ever touched - so
// linking the whole history is sound, and is what pulls coverage up from the
// ~63% the backfill can reach as agents come back.
//
// Only rows with no player at all are touched. An existing attribution is never
// moved: if the nickname already belongs to a different player record the whole
// claim is abandoned and reported as a conflict.
//
// That guard is load-bearing, not defensive tidiness. The upload route is
// unauthenticated and the nickname is self-asserted, so without it anyone with
// C.O.R.E. could post one media under a well-known agent's name and walk off
// with their entire upload history. Refusing to touch already-attributed rows
// means everything the backfill assigned - and everything any agent has since
// claimed - is out of reach. What remains exposed is a name that has never been
// attributed to anyone, which is why verification, not this, is what ultimately
// makes attribution trustworthy.
//
// Raw SQL rather than app.Save, for the same reason the backfill uses it:
// PocketBase stamps "updated" on every save, and moderators use that field to
// find records still awaiting curation.
func ClaimHistory(app core.App, playerRecordID, ign string) (ClaimResult, error) {
	// "UNKNOWN" is the marker a moderator leaves behind when scrubbing an
	// upload's attribution, so it names no one and must never be claimed.
	if playerRecordID == "" || ign == "" || ign == "UNKNOWN" {
		return ClaimResult{}, nil
	}

	var result ClaimResult

	err := app.RunInTransaction(func(txApp core.App) error {
		conflict, err := ignClaimedElsewhere(txApp, playerRecordID, ign)
		if err != nil {
			return err
		}
		if conflict {
			result.Conflict = true
			return nil
		}

		for _, collection := range linkedCollections {
			// PocketBase writes an unset relation as '' rather than NULL, but
			// rows created before the field existed are genuinely NULL - both
			// mean unattributed.
			res, err := txApp.DB().
				NewQuery("UPDATE " + collection + " SET player = {:player} " +
					"WHERE uploader_ign = {:ign} AND (player IS NULL OR player = '')").
				Bind(dbx.Params{"player": playerRecordID, "ign": ign}).
				Execute()
			if err != nil {
				return fmt.Errorf("claiming %s rows for %q: %w", collection, ign, err)
			}

			affected, err := res.RowsAffected()
			if err != nil {
				return err
			}
			result.Linked += int(affected)
		}

		return nil
	})
	if err != nil {
		return ClaimResult{}, err
	}

	return result, nil
}

// ignClaimedElsewhere reports whether any row under this nickname is already
// attributed to a player other than the one asking for it.
func ignClaimedElsewhere(app core.App, playerRecordID, ign string) (bool, error) {
	for _, collection := range linkedCollections {
		var out struct {
			Count int `db:"count"`
		}
		if err := app.DB().
			NewQuery("SELECT COUNT(*) count FROM " + collection + " " +
				"WHERE uploader_ign = {:ign} AND player IS NOT NULL AND player != '' AND player != {:player}").
			Bind(dbx.Params{"player": playerRecordID, "ign": ign}).
			One(&out); err != nil {
			return false, fmt.Errorf("checking %s attribution for %q: %w", collection, ign, err)
		}
		if out.Count > 0 {
			return true, nil
		}
	}

	return false, nil
}
