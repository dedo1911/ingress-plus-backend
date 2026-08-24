package backfill

import "testing"

// TestCollectHashesCoversUntrustedRecords is the difference between
// collectHashes and buildMappings: a player ID that appears only in the legacy
// import can never be attributed to a username, but it is still a real account
// and its hash is stable, so it has to survive the backfill. When that agent
// uploads again the live path derives the same hash, finds the record already
// waiting, and their history can be linked up from there.
func TestCollectHashesCoversUntrustedRecords(t *testing.T) {
	hasher := testHasher(t)

	rows := []mediaRow{
		// plugin-era, attributable
		{ID: "1", UploaderIgn: "agent", Created: "2026-01-01 00:00:00.000Z", OriginalData: payload(playerA)},
		// legacy only - no trustworthy username, but a real player ID
		{ID: "2", UploaderIgn: "whoever", Created: "2024-01-11 07:12:27.937Z", OriginalData: payload(playerB)},
		// scrubbed, must not become a hash
		{ID: "3", UploaderIgn: "UNKNOWN", Created: "2024-01-11 07:12:28.000Z", OriginalData: payload("USER DELETED")},
		{ID: "4", UploaderIgn: "kidobarrett", Created: "2024-01-11 07:12:29.000Z", OriginalData: payload("")},
	}

	got := collectHashes(rows, hasher)
	if len(got) != 2 {
		t.Fatalf("collectHashes returned %d hashes, want 2 (both real player IDs)", len(got))
	}

	trusted, _ := buildMappings(rows, hasher)
	if len(trusted) != 1 {
		t.Fatalf("buildMappings returned %d mappings, want 1 (only the plugin-era row)", len(trusted))
	}

	// the legacy-only player is hashed but has no username mapping
	wantB, err := hasher.Hash(playerB)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range got {
		if h == wantB {
			found = true
		}
	}
	if !found {
		t.Fatal("the legacy-only player ID was dropped instead of being hashed")
	}
	if trusted[0].hash == wantB {
		t.Fatal("the legacy-only player ID was attributed to a username")
	}
}

func TestCollectHashesIsDeduplicatedAndSorted(t *testing.T) {
	hasher := testHasher(t)

	rows := []mediaRow{
		{ID: "1", UploaderIgn: "a", Created: "2026-01-01 00:00:00.000Z", OriginalData: payload(playerA)},
		{ID: "2", UploaderIgn: "a", Created: "2026-01-02 00:00:00.000Z", OriginalData: payload(playerA)},
		{ID: "3", UploaderIgn: "b", Created: "2026-01-03 00:00:00.000Z", OriginalData: payload(playerB)},
	}

	got := collectHashes(rows, hasher)
	if len(got) != 2 {
		t.Fatalf("got %d hashes, want 2 distinct", len(got))
	}
	if got[0] > got[1] {
		t.Fatalf("hashes are not sorted: %v", got)
	}
}
