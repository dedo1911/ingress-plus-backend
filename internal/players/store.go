package players

import (
	"database/sql"
	"errors"

	"github.com/pocketbase/pocketbase/core"
)

// CollectionName is the collection holding one record per Ingress account.
const CollectionName = "players"

// Ensure returns the "players" record for the given hash, creating it on first
// sight, and keeps the denormalized nickname/faction current.
//
// last_ign is overwritten rather than merged because an agent's username can
// change while their player ID cannot: whichever nickname we saw most recently
// is the current one. It exists only so the admin panel has something readable
// to match a COMM message against - the hash stays the identity.
//
// An empty ign or faction leaves whatever is already stored alone. The backfill
// creates records for player IDs it deliberately refuses to attribute and has
// no trustworthy nickname to offer for them; that must not erase a nickname
// another record already proved.
//
// Creation races on the unique player_hash index (two uploads from the same
// agent arriving together) are resolved by re-reading rather than failing: by
// then the other writer has created exactly the record we wanted.
func Ensure(app core.App, hash, ign, faction string) (*core.Record, error) {
	record, err := app.FindFirstRecordByData(CollectionName, "player_hash", hash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	if record == nil {
		collection, err := app.FindCollectionByNameOrId(CollectionName)
		if err != nil {
			return nil, err
		}

		record = core.NewRecord(collection)
		record.Set("player_hash", hash)
		record.Set("last_ign", ign)
		record.Set("last_faction", faction)

		if err := app.Save(record); err != nil {
			existing, findErr := app.FindFirstRecordByData(CollectionName, "player_hash", hash)
			if findErr != nil || existing == nil {
				return nil, err
			}
			return existing, nil
		}
		return record, nil
	}

	currentIgn := record.GetString("last_ign")
	currentFaction := record.GetString("last_faction")

	newIgn := currentIgn
	if ign != "" {
		newIgn = ign
	}
	newFaction := currentFaction
	if faction != "" {
		newFaction = faction
	}

	if newIgn == currentIgn && newFaction == currentFaction {
		return record, nil
	}

	record.Set("last_ign", newIgn)
	record.Set("last_faction", newFaction)
	if err := app.Save(record); err != nil {
		return nil, err
	}
	return record, nil
}
