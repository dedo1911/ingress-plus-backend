package backfill

import (
	"testing"

	"github.com/dedo1911/ingress-plus-backend/internal/players"
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

func TestBuildMappingsCountsRecordsForTrustThreshold(t *testing.T) {
	var rows []mediaRow
	for i := 0; i < minRecordsToTrustMapping; i++ {
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
	if counts["prolific"] < minRecordsToTrustMapping {
		t.Fatalf("prolific agent counted %d records, want at least %d", counts["prolific"], minRecordsToTrustMapping)
	}
	if counts["occasional"] >= minRecordsToTrustMapping {
		t.Fatalf("occasional agent should fall below the trust threshold, counted %d", counts["occasional"])
	}
}
