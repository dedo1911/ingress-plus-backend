// Package backfill holds one-off data migrations that are run deliberately
// from the CLI rather than automatically on boot.
package backfill

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/dedo1911/ingress-plus-backend/internal/players"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"
)

// legacyImportDate is the day the whole mediagress.net archive was imported.
// Those records' original_data does not correspond to their uploader_ign - one
// username carries up to 12 different player IDs and vice versa - so they are
// useless for working out who an agent is. Everything uploaded by the plugin
// since is perfectly consistent, so the username -> player mapping is derived
// from those records only.
const legacyImportDate = "2024-01-11"

// minRecordsToTrustMapping is how many consistent records a username needs
// before its player ID is taken as proven. Below this an admin assigns it by
// hand from the report this command prints.
const minRecordsToTrustMapping = 3

type mediaRow struct {
	ID           string `db:"id"`
	UploaderIgn  string `db:"uploader_ign"`
	Created      string `db:"created"`
	OriginalData string `db:"original_data"`
}

type mapping struct {
	hash    string
	ign     string
	records int
	latest  string
}

// NewCommand builds the "backfill-player-hashes" CLI command.
//
// It does three things, in order: derive a trustworthy username -> player-hash
// map from the plugin-era records, create a players record for every player ID
// in the table and attach the trusted ones to medias/media_uploads by username,
// and strip every raw player ID out of original_data.
//
// Attribution is applied by username rather than from each record's own
// original_data. That sounds indirect but is the only correct option: an
// Ingress username, once used, is never released to anyone else, so a
// (username, player) pair proven from good records holds for every record that
// username ever touched - including the legacy ones whose own original_data
// cannot be trusted, and including media_uploads, which never stored a player
// ID at all.
func NewCommand(app *pocketbase.PocketBase) *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "backfill-player-hashes",
		Short: "Hash player IDs out of medias.original_data and attach player records",
		Run: func(cmd *cobra.Command, args []string) {
			hasher, err := players.NewHasherFromEnv()
			if err != nil {
				cmd.PrintErrln("error:", err)
				return
			}
			if err := run(app, hasher, dryRun, cmd); err != nil {
				cmd.PrintErrln("error:", err)
			}
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change without writing anything")

	return cmd
}

func run(app core.App, hasher *players.Hasher, dryRun bool, cmd *cobra.Command) error {
	var rows []mediaRow
	if err := app.DB().
		NewQuery("SELECT id, uploader_ign, created, original_data FROM medias").
		All(&rows); err != nil {
		return fmt.Errorf("reading medias: %w", err)
	}

	trusted, skipped := buildMappings(rows, hasher)
	hashes := collectHashes(rows, hasher)

	auto := map[string]mapping{}
	var review []mapping
	for _, m := range trusted {
		if m.records >= minRecordsToTrustMapping {
			auto[m.ign] = m
			continue
		}
		review = append(review, m)
	}

	cmd.Printf("medias scanned:            %d\n", len(rows))
	cmd.Printf("scrubbed / no player ID:   %d\n", skipped)
	cmd.Printf("distinct player IDs:       %d\n", len(hashes))
	cmd.Printf("trusted username mappings: %d (%d auto-assignable, %d for manual review)\n",
		len(trusted), len(auto), len(review))

	if len(review) > 0 {
		sort.Slice(review, func(i, j int) bool { return review[i].ign < review[j].ign })
		cmd.Println("\nbelow the trust threshold - assign these by hand in the admin panel:")
		for _, m := range review {
			cmd.Printf("  %-18s %s  (%d record(s))\n", m.ign, m.hash[:16], m.records)
		}
	}

	if dryRun {
		cmd.Println("\ndry run - nothing written")
		return nil
	}

	// Every distinct player ID gets a players record - the trusted mappings
	// carry their nickname, the rest are created bare and stay unlinked.
	hashToIgn := map[string]string{}
	for _, m := range trusted {
		hashToIgn[m.hash] = m.ign
	}

	hashToRecordID := map[string]string{}
	for _, hash := range hashes {
		record, err := players.Ensure(app, hash, hashToIgn[hash], "")
		if err != nil {
			return fmt.Errorf("creating player record for %s: %w", hash[:16], err)
		}
		hashToRecordID[hash] = record.Id
	}

	// Only mappings above the trust threshold are attached to records;
	// everything else waits for a human.
	ignToRecordID := map[string]string{}
	for _, m := range auto {
		ignToRecordID[m.ign] = hashToRecordID[m.hash]
	}

	cmd.Printf("\nplayers records ensured:   %d (%d identified, %d unattributed)\n",
		len(hashToRecordID), len(trusted), len(hashToRecordID)-len(trusted))

	mediasLinked, err := linkByIgn(app, "medias", ignToRecordID)
	if err != nil {
		return err
	}
	uploadsLinked, err := linkByIgn(app, "media_uploads", ignToRecordID)
	if err != nil {
		return err
	}
	cmd.Printf("medias linked:             %d\n", mediasLinked)
	cmd.Printf("media_uploads linked:      %d\n", uploadsLinked)

	stripped, err := stripPlayerIDs(app, rows)
	if err != nil {
		return err
	}
	cmd.Printf("raw player IDs stripped:   %d\n", stripped)

	return nil
}

// buildMappings derives one username -> player-hash mapping per username from
// the records that can be trusted, and returns how many records were skipped.
//
// A username that somehow maps to more than one player is dropped entirely
// rather than guessed at; in the current production data none do.
func buildMappings(rows []mediaRow, hasher *players.Hasher) ([]mapping, int) {
	type counter struct {
		records int
		latest  string
	}

	byIgn := map[string]map[string]*counter{}
	skipped := 0

	for _, row := range rows {
		raw := rawPlayerID(row.OriginalData)
		if !players.IsPlayerID(raw) || row.UploaderIgn == "" || row.UploaderIgn == "UNKNOWN" {
			skipped++
			continue
		}
		if len(row.Created) >= len(legacyImportDate) && row.Created[:len(legacyImportDate)] == legacyImportDate {
			continue
		}

		hash, err := hasher.Hash(raw)
		if err != nil {
			skipped++
			continue
		}

		if byIgn[row.UploaderIgn] == nil {
			byIgn[row.UploaderIgn] = map[string]*counter{}
		}
		c := byIgn[row.UploaderIgn][hash]
		if c == nil {
			c = &counter{}
			byIgn[row.UploaderIgn][hash] = c
		}
		c.records++
		if row.Created > c.latest {
			c.latest = row.Created
		}
	}

	var out []mapping
	for ign, hashes := range byIgn {
		if len(hashes) != 1 {
			continue
		}
		for hash, c := range hashes {
			out = append(out, mapping{hash: hash, ign: ign, records: c.records, latest: c.latest})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ign < out[j].ign })

	return out, skipped
}

// collectHashes returns every distinct player hash appearing anywhere in the
// table, legacy import included, sorted so a run is deterministic.
//
// Deliberately wider than buildMappings. A player ID that only the
// untrustworthy legacy import mentions cannot be attributed to anyone, but it
// is still a real account and the hash is stable: if that agent ever uploads
// again the live path derives the same hash and lands on the same record, at
// which point their history can be linked up. Discarding it at backfill time
// would throw that future away for nothing, since the raw ID is being stripped
// either way.
func collectHashes(rows []mediaRow, hasher *players.Hasher) []string {
	seen := map[string]bool{}
	for _, row := range rows {
		hash, err := hasher.Hash(rawPlayerID(row.OriginalData))
		if err != nil {
			continue
		}
		seen[hash] = true
	}

	out := make([]string, 0, len(seen))
	for hash := range seen {
		out = append(out, hash)
	}
	sort.Strings(out)

	return out
}

// rawPlayerID digs the player ID out of a stored original_data blob. Anything
// unparseable yields "", which IsPlayerID then rejects.
func rawPlayerID(originalData string) string {
	var payload struct {
		InInventory struct {
			PlayerID string `json:"playerId"`
		} `json:"inInventory"`
	}
	if err := json.Unmarshal([]byte(originalData), &payload); err != nil {
		return ""
	}
	return payload.InInventory.PlayerID
}

// linkByIgn points a collection's player relation at the right players record.
//
// Raw SQL, not app.Save: PocketBase stamps the updated field on every save, and
// rewriting it across ~13k rows would destroy the signal moderators use to find
// records still awaiting curation.
func linkByIgn(app core.App, collection string, ignToRecordID map[string]string) (int, error) {
	total := 0
	for ign, recordID := range ignToRecordID {
		result, err := app.DB().
			NewQuery("UPDATE " + collection + " SET player = {:player} WHERE uploader_ign = {:ign} AND (player IS NULL OR player = '')").
			Bind(dbx.Params{"player": recordID, "ign": ign}).
			Execute()
		if err != nil {
			return total, fmt.Errorf("linking %s for %s: %w", collection, ign, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return total, err
		}
		total += int(affected)
	}
	return total, nil
}

// stripPlayerIDs removes the raw ID from every record that has a real one,
// trusted or not - the privacy fix applies to all of them, including the legacy
// import whose IDs are real even though its attribution is not.
//
// Records whose playerId is not a well-formed ID are left exactly as they are.
// Those are deliberate moderator scrubs (currently one "USER DELETED" and one
// empty string): there is nothing left in them to protect, and overwriting the
// marker would erase the evidence that the removal was intentional rather than
// data that never had a player ID in the first place.
func stripPlayerIDs(app core.App, rows []mediaRow) (int, error) {
	stripped := 0

	for _, row := range rows {
		if !players.IsPlayerID(rawPlayerID(row.OriginalData)) {
			continue
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(row.OriginalData), &payload); err != nil {
			continue
		}
		inventory, ok := payload["inInventory"].(map[string]any)
		if !ok {
			continue
		}
		delete(inventory, "playerId")

		cleaned, err := json.Marshal(payload)
		if err != nil {
			return stripped, err
		}

		if _, err := app.DB().
			NewQuery("UPDATE medias SET original_data = {:data} WHERE id = {:id}").
			Bind(dbx.Params{"data": string(cleaned), "id": row.ID}).
			Execute(); err != nil {
			return stripped, fmt.Errorf("stripping player ID from %s: %w", row.ID, err)
		}
		stripped++
	}

	return stripped, nil
}
