package backfill

import (
	"strings"
	"testing"

	"github.com/dedo1911/ingress-plus-backend/internal/players"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/spf13/cobra"
)

const (
	playerA = "cb3773292130450080439d75cdcff215.c"
	playerB = "bdad80740e154097ab7aa238da864c03.c"
)

func payload(playerID string) string {
	if playerID == "" {
		return `{"inInventory":{"acquisitionTimestampMs":"1785551001103"},"storyItem":{"mediaId":"1"}}`
	}
	return `{"inInventory":{"playerId":"` + playerID + `","acquisitionTimestampMs":"1785551001103"},"storyItem":{"mediaId":"1"}}`
}

func testHasher(t *testing.T) *players.Hasher {
	t.Helper()
	hasher, err := players.NewHasher("test-pepper")
	if err != nil {
		t.Fatal(err)
	}
	return hasher
}

func TestRawPlayerID(t *testing.T) {
	if got := rawPlayerID(payload(playerA)); got != playerA {
		t.Fatalf("rawPlayerID() = %q, want %q", got, playerA)
	}
	if got := rawPlayerID(payload("")); got != "" {
		t.Fatalf("rawPlayerID() on a payload without one = %q, want empty", got)
	}
	if got := rawPlayerID("not json"); got != "" {
		t.Fatalf("rawPlayerID() on garbage = %q, want empty", got)
	}
	if got := rawPlayerID(payload("USER DELETED")); got != "USER DELETED" {
		t.Fatalf("rawPlayerID() should return the scrub marker verbatim, got %q", got)
	}
}

func TestBuildMappingsIgnoresLegacyImport(t *testing.T) {
	// The 2024-01-11 import's original_data does not correspond to its
	// uploader_ign, so it must not contribute to the mapping even though the
	// player IDs in it are well-formed. Here the legacy rows would claim
	// "agent" is playerB; only the plugin-era row is authoritative.
	rows := []mediaRow{
		{ID: "1", UploaderIgn: "agent", Created: "2024-01-11 07:12:27.937Z", OriginalData: payload(playerB)},
		{ID: "2", UploaderIgn: "agent", Created: "2024-01-11 07:12:28.000Z", OriginalData: payload(playerB)},
		{ID: "3", UploaderIgn: "agent", Created: "2026-05-20 10:00:00.000Z", OriginalData: payload(playerA)},
	}

	got, _ := buildMappings(rows, testHasher(t))
	if len(got) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(got))
	}
	if got[0].ign != "agent" {
		t.Fatalf("mapping ign = %q, want %q", got[0].ign, "agent")
	}
	if got[0].records != 1 {
		t.Fatalf("legacy rows leaked into the record count: got %d, want 1", got[0].records)
	}

	wantHash, err := testHasher(t).Hash(playerA)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].hash != wantHash {
		t.Fatal("mapping used the legacy player ID instead of the plugin-era one")
	}
}

func TestBuildMappingsSkipsScrubbedRecords(t *testing.T) {
	rows := []mediaRow{
		{ID: "1", UploaderIgn: "kidobarrett", Created: "2026-01-01 00:00:00.000Z", OriginalData: payload("")},
		{ID: "2", UploaderIgn: "UNKNOWN", Created: "2026-01-01 00:00:00.000Z", OriginalData: payload("USER DELETED")},
		{ID: "3", UploaderIgn: "", Created: "2026-01-01 00:00:00.000Z", OriginalData: payload(playerA)},
		{ID: "4", UploaderIgn: "agent", Created: "2026-01-01 00:00:00.000Z", OriginalData: payload(playerA)},
	}

	got, skipped := buildMappings(rows, testHasher(t))
	if skipped != 3 {
		t.Fatalf("skipped = %d, want 3", skipped)
	}
	if len(got) != 1 || got[0].ign != "agent" {
		t.Fatalf("expected only the clean row to map, got %+v", got)
	}
}

// TestBuildMappingsDropsContradictions guards the rule that makes attribution
// by username safe. A username is never released to another agent, so one
// username resolving to two players means the data is wrong, not that the
// agent changed - guessing either way would misattribute their uploads.
func TestBuildMappingsDropsContradictions(t *testing.T) {
	rows := []mediaRow{
		{ID: "1", UploaderIgn: "agent", Created: "2026-01-01 00:00:00.000Z", OriginalData: payload(playerA)},
		{ID: "2", UploaderIgn: "agent", Created: "2026-01-02 00:00:00.000Z", OriginalData: payload(playerB)},
		{ID: "3", UploaderIgn: "clean", Created: "2026-01-03 00:00:00.000Z", OriginalData: payload(playerA)},
	}

	got, _ := buildMappings(rows, testHasher(t))
	if len(got) != 1 {
		t.Fatalf("expected the contradictory username to be dropped, got %+v", got)
	}
	if got[0].ign != "clean" {
		t.Fatalf("kept the wrong mapping: %+v", got[0])
	}
}

// A renamed agent is the mirror image and must be kept: one player showing up
// under several usernames is expected, since each of those usernames belonged
// to them and to nobody else.
func TestBuildMappingsKeepsRenames(t *testing.T) {
	rows := []mediaRow{
		{ID: "1", UploaderIgn: "oldname", Created: "2025-01-01 00:00:00.000Z", OriginalData: payload(playerA)},
		{ID: "2", UploaderIgn: "newname", Created: "2026-01-01 00:00:00.000Z", OriginalData: payload(playerA)},
	}

	got, _ := buildMappings(rows, testHasher(t))
	if len(got) != 2 {
		t.Fatalf("expected both usernames to map to the same player, got %+v", got)
	}
	if got[0].hash != got[1].hash {
		t.Fatal("a renamed agent's usernames mapped to different players")
	}
}

// Record counts are no longer a gate - every mapping is assigned - but the
// report still calls out the thinly evidenced ones, so the count has to be
// right.
func TestBuildMappingsCountsRecords(t *testing.T) {
	var rows []mediaRow
	for i := 0; i < 3; i++ {
		rows = append(rows, mediaRow{
			ID:           string(rune('a' + i)),
			UploaderIgn:  "prolific",
			Created:      "2026-01-01 00:00:00.000Z",
			OriginalData: payload(playerA),
		})
	}
	rows = append(rows, mediaRow{ID: "z", UploaderIgn: "occasional", Created: "2026-01-01 00:00:00.000Z", OriginalData: payload(playerB)})

	got, _ := buildMappings(rows, testHasher(t))

	counts := map[string]int{}
	for _, m := range got {
		counts[m.ign] = m.records
	}
	if counts["prolific"] != 3 {
		t.Fatalf("prolific agent counted %d records, want 3", counts["prolific"])
	}
	if counts["occasional"] != 1 {
		t.Fatalf("occasional agent counted %d records, want 1", counts["occasional"])
	}
	if len(got) != 2 {
		t.Fatalf("both agents should be assigned, got %d mappings", len(got))
	}
}

// Faction follows the newest record for the same reason the nickname does: an
// agent can switch, and whatever we saw last is the current one.
func TestBuildMappingsTakesTheNewestFaction(t *testing.T) {
	rows := []mediaRow{
		{ID: "1", UploaderIgn: "switcher", Faction: "ENLIGHTENED", Created: "2025-01-01 00:00:00.000Z", OriginalData: payload(playerA)},
		{ID: "2", UploaderIgn: "switcher", Faction: "RESISTANCE", Created: "2026-01-01 00:00:00.000Z", OriginalData: payload(playerA)},
	}

	got, _ := buildMappings(rows, testHasher(t))
	if len(got) != 1 {
		t.Fatalf("expected one mapping, got %+v", got)
	}
	if got[0].faction != "RESISTANCE" {
		t.Fatalf("faction = %q, want the newest record's RESISTANCE", got[0].faction)
	}
}

// TestStripPlayerIDsLeavesScrubMarkersAlone checks that the privacy pass
// rewrites records holding a real player ID and nothing else. A moderator's
// scrub marker is evidence that a removal was deliberate, and overwriting it
// would make that indistinguishable from a record that never had an ID.
func TestStripPlayerIDsLeavesScrubMarkersAlone(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	medias := core.NewBaseCollection("medias")
	medias.Fields.Add(&core.JSONField{Name: "original_data", MaxSize: 2000000})
	if err := app.Save(medias); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		payload string
		changed bool
	}{
		{"real player ID", payload(playerA), true},
		{"scrubbed to a marker", payload("USER DELETED"), false},
		{"scrubbed to empty", payload(""), false},
	}

	var rows []mediaRow
	ids := map[string]string{}
	for _, c := range cases {
		record := core.NewRecord(medias)
		record.Set("original_data", c.payload)
		if err := app.Save(record); err != nil {
			t.Fatal(err)
		}
		ids[c.name] = record.Id
		rows = append(rows, mediaRow{ID: record.Id, OriginalData: c.payload})
	}

	stripped, err := stripPlayerIDs(app, rows)
	if err != nil {
		t.Fatal(err)
	}
	if stripped != 1 {
		t.Fatalf("stripped = %d, want 1 (only the record with a real player ID)", stripped)
	}

	for _, c := range cases {
		record, err := app.FindRecordById("medias", ids[c.name])
		if err != nil {
			t.Fatal(err)
		}
		stored := record.GetString("original_data")

		if c.changed {
			if strings.Contains(stored, playerA) {
				t.Errorf("%s: raw player ID survived: %s", c.name, stored)
			}
			if strings.Contains(stored, "playerId") {
				t.Errorf("%s: playerId key survived: %s", c.name, stored)
			}
			if !strings.Contains(stored, "acquisitionTimestampMs") {
				t.Errorf("%s: stripping removed more than the player ID: %s", c.name, stored)
			}
			continue
		}

		if rawPlayerID(stored) != rawPlayerID(c.payload) {
			t.Errorf("%s: marker was modified, got %s", c.name, stored)
		}
	}
}

// TestRunLinksThinlyEvidencedAgents is the end-to-end check the first two
// production runs went without: an agent proven by a single plugin-era record
// must come out linked, with their faction recorded. Both were defects - the
// old trust threshold withheld 95 medias and 1642 upload rows from agents like
// this one, and the faction was never written at all.
func TestRunLinksThinlyEvidencedAgents(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	playersCollection := core.NewBaseCollection(players.CollectionName)
	playersCollection.Fields.Add(&core.TextField{Name: "player_hash"})
	playersCollection.Fields.Add(&core.TextField{Name: "last_ign"})
	playersCollection.Fields.Add(&core.TextField{Name: "last_faction"})
	playersCollection.AddIndex("idx_players_hash", true, "player_hash", "")
	if err := app.Save(playersCollection); err != nil {
		t.Fatal(err)
	}

	medias := core.NewBaseCollection("medias")
	medias.Fields.Add(&core.TextField{Name: "uploader_ign"})
	medias.Fields.Add(&core.TextField{Name: "uploader_faction"})
	medias.Fields.Add(&core.JSONField{Name: "original_data", MaxSize: 2000000})
	medias.Fields.Add(&core.RelationField{Name: "player", CollectionId: playersCollection.Id, MaxSelect: 1})
	// buildMappings reads created to tell plugin-era rows from the legacy import.
	medias.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	if err := app.Save(medias); err != nil {
		t.Fatal(err)
	}

	uploads := core.NewBaseCollection("media_uploads")
	uploads.Fields.Add(&core.TextField{Name: "media_url_id"})
	uploads.Fields.Add(&core.TextField{Name: "uploader_ign"})
	uploads.Fields.Add(&core.RelationField{Name: "player", CollectionId: playersCollection.Id, MaxSelect: 1})
	if err := app.Save(uploads); err != nil {
		t.Fatal(err)
	}

	// One plugin-era discovery, which is all the evidence there is for this
	// agent, plus upload rows that only carry their nickname.
	media := core.NewRecord(medias)
	media.Set("uploader_ign", "occasional")
	media.Set("uploader_faction", "ENLIGHTENED")
	media.Set("original_data", payload(playerA))
	if err := app.Save(media); err != nil {
		t.Fatal(err)
	}
	for _, urlID := range []string{"1", "2"} {
		row := core.NewRecord(uploads)
		row.Set("media_url_id", urlID)
		row.Set("uploader_ign", "occasional")
		if err := app.Save(row); err != nil {
			t.Fatal(err)
		}
	}

	if err := run(app, testHasher(t), false, &cobra.Command{}); err != nil {
		t.Fatal(err)
	}

	player, err := app.FindFirstRecordByData(players.CollectionName, "last_ign", "occasional")
	if err != nil {
		t.Fatalf("no players record was created for a single-record agent: %v", err)
	}
	if got := player.GetString("last_faction"); got != "ENLIGHTENED" {
		t.Fatalf("last_faction = %q, want ENLIGHTENED", got)
	}

	linked, err := app.FindRecordById("medias", media.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got := linked.GetString("player"); got != player.Id {
		t.Fatalf("medias.player = %q, want %q", got, player.Id)
	}

	rows, err := app.FindAllRecords("media_uploads")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if got := row.GetString("player"); got != player.Id {
			t.Fatalf("media_uploads row %s left unlinked: player = %q", row.Id, got)
		}
	}
}
