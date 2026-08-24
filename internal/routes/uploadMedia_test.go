package routes

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dedo1911/ingress-plus-backend/internal/players"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

const testPlayerID = "cb3773292130450080439d75cdcff215.c"

// newTestApp builds the three collections the upload path touches. Their shapes
// mirror production closely enough for the behaviour under test; the live
// schema lives in PocketBase, not in this repo.
func newTestApp(t *testing.T) *tests.TestApp {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	playersCollection := core.NewBaseCollection(players.CollectionName)
	playersCollection.Fields.Add(&core.TextField{Name: "player_hash"})
	playersCollection.Fields.Add(&core.TextField{Name: "last_ign"})
	playersCollection.Fields.Add(&core.TextField{Name: "last_faction"})
	playersCollection.AddIndex("idx_players_hash", true, "player_hash", "")
	if err := app.Save(playersCollection); err != nil {
		t.Fatalf("creating players collection: %v", err)
	}

	medias := core.NewBaseCollection("medias")
	medias.Fields.Add(&core.NumberField{Name: "url_id"})
	medias.Fields.Add(&core.TextField{Name: "media_id"})
	medias.Fields.Add(&core.TextField{Name: "image_url"})
	medias.Fields.Add(&core.TextField{Name: "content_url"})
	medias.Fields.Add(&core.TextField{Name: "short_description"})
	medias.Fields.Add(&core.TextField{Name: "description"})
	medias.Fields.Add(&core.DateField{Name: "released_at"})
	medias.Fields.Add(&core.TextField{Name: "uploader_ign"})
	medias.Fields.Add(&core.TextField{Name: "uploader_faction"})
	medias.Fields.Add(&core.JSONField{Name: "original_data", MaxSize: 2000000})
	medias.Fields.Add(&core.NumberField{Name: "level"})
	medias.Fields.Add(&core.BoolField{Name: "approved"})
	medias.Fields.Add(&core.RelationField{Name: "player", CollectionId: playersCollection.Id, MaxSelect: 1})
	if err := app.Save(medias); err != nil {
		t.Fatalf("creating medias collection: %v", err)
	}

	uploads := core.NewBaseCollection("media_uploads")
	uploads.Fields.Add(&core.TextField{Name: "media_url_id"})
	uploads.Fields.Add(&core.TextField{Name: "uploader_ign"})
	uploads.Fields.Add(&core.TextField{Name: "uploader_faction"})
	uploads.Fields.Add(&core.RelationField{Name: "player", CollectionId: playersCollection.Id, MaxSelect: 1})
	uploads.AddIndex("idx_media_uploads_unique", true, "media_url_id, uploader_ign", "")
	if err := app.Save(uploads); err != nil {
		t.Fatalf("creating media_uploads collection: %v", err)
	}

	return app
}

func testMedia(mediaID string) Media {
	var media Media
	media.InInventory.PlayerID = testPlayerID
	media.InInventory.AcquisitionTimestampMs = "1785551001103"
	media.ResourceWithLevels.ResourceType = "MEDIA"
	media.ResourceWithLevels.Level = 1
	media.ImageByURL.ImageURL = "http://example.invalid/image.png"
	media.StoryItem.MediaID = mediaID
	media.StoryItem.PrimaryURL = "http://example.invalid/story"
	media.StoryItem.ShortDescription = "test media " + mediaID
	media.StoryItem.ReleaseDate = "1782748800000"
	return media
}

func testPlayer(nickname string) Player {
	return Player{Nickname: nickname, Team: "RESISTANCE"}
}

func TestStripPlayerIDLeavesNoTrace(t *testing.T) {
	media := testMedia("5332")

	encoded, err := json.Marshal(stripPlayerID(media))
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(encoded), testPlayerID) {
		t.Fatalf("raw player ID survived into stored payload: %s", encoded)
	}
	// omitempty should drop the key entirely rather than leave "playerId":""
	if strings.Contains(string(encoded), "playerId") {
		t.Fatalf("playerId key survived into stored payload: %s", encoded)
	}
	// the rest of the payload must be preserved
	if !strings.Contains(string(encoded), "acquisitionTimestampMs") {
		t.Fatalf("stripping removed more than the player ID: %s", encoded)
	}

	// the caller's copy is untouched, so the ID is still available for hashing
	if media.InInventory.PlayerID != testPlayerID {
		t.Fatal("stripPlayerID mutated the caller's Media")
	}
}

func TestEnsureMediaCreatesOnceAndStripsPlayerID(t *testing.T) {
	app := newTestApp(t)
	media := testMedia("5332")
	player := testPlayer("oscarc1")

	urlID, created, err := ensureMedia(app, media, "", player, true)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first upload of a Media should have created it")
	}
	if urlID != 1 {
		t.Fatalf("expected the first url_id to be 1, got %d", urlID)
	}

	record, err := app.FindFirstRecordByData("medias", "media_id", "5332")
	if err != nil {
		t.Fatal(err)
	}
	if stored := record.GetString("original_data"); strings.Contains(stored, testPlayerID) {
		t.Fatalf("raw player ID was persisted: %s", stored)
	}

	// a second upload of the same Media must not create another record
	sameURLID, created, err := ensureMedia(app, media, "", testPlayer("someoneelse"), true)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("an already-known Media was created a second time")
	}
	if sameURLID != urlID {
		t.Fatalf("expected url_id %d for the known Media, got %d", urlID, sameURLID)
	}
}

// TestEnsureUploadLogsEveryAgent covers the regression this change exists for:
// re-uploading a Media that Mediagress already knows about produced no
// media_uploads row at all, so repeat contributions went unrecorded and
// "new to you" could never be reported.
func TestEnsureUploadLogsEveryAgent(t *testing.T) {
	app := newTestApp(t)

	firstTime, err := ensureUpload(app, 1, "", testPlayer("oscarc1"))
	if err != nil {
		t.Fatal(err)
	}
	if !firstTime {
		t.Fatal("an agent's first upload of a Media should count as a first time")
	}

	// same agent, same Media - logged once, not twice
	firstTime, err = ensureUpload(app, 1, "", testPlayer("oscarc1"))
	if err != nil {
		t.Fatal(err)
	}
	if firstTime {
		t.Fatal("re-uploading the same Media counted as a first time again")
	}

	// a different agent uploading a Media that already exists is the case that
	// used to be dropped entirely
	firstTime, err = ensureUpload(app, 1, "", testPlayer("dai02"))
	if err != nil {
		t.Fatal(err)
	}
	if !firstTime {
		t.Fatal("a second agent's first upload of a known Media was not recorded")
	}

	rows, err := app.FindAllRecords("media_uploads")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 media_uploads rows (one per agent), got %d", len(rows))
	}
}

func TestEnsureUploadBackfillsPlayerOnExistingRow(t *testing.T) {
	app := newTestApp(t)
	player := testPlayer("oscarc1")

	// a row from before player attribution existed
	if _, err := ensureUpload(app, 1, "", player); err != nil {
		t.Fatal(err)
	}

	hasher, err := players.NewHasher("test-pepper")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hasher.Hash(testPlayerID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := players.Ensure(app, hash, player.Nickname, player.Team)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ensureUpload(app, 1, record.Id, player); err != nil {
		t.Fatal(err)
	}

	rows, err := app.FindAllRecords("media_uploads")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the existing row to be reused, got %d rows", len(rows))
	}
	if got := rows[0].GetString("player"); got != record.Id {
		t.Fatalf("existing row was not linked to the player: got %q, want %q", got, record.Id)
	}
}

func TestEnsurePlayerIsIdempotentAndTracksRenames(t *testing.T) {
	app := newTestApp(t)

	hasher, err := players.NewHasher("test-pepper")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hasher.Hash(testPlayerID)
	if err != nil {
		t.Fatal(err)
	}

	first, err := players.Ensure(app, hash, "oldname", "RESISTANCE")
	if err != nil {
		t.Fatal(err)
	}

	// an agent renamed: same player ID, new username - the record must be
	// reused and the nickname refreshed, not duplicated
	second, err := players.Ensure(app, hash, "newname", "RESISTANCE")
	if err != nil {
		t.Fatal(err)
	}
	if first.Id != second.Id {
		t.Fatalf("a renamed agent got a second players record: %q then %q", first.Id, second.Id)
	}
	if got := second.GetString("last_ign"); got != "newname" {
		t.Fatalf("last_ign = %q, want the most recent nickname %q", got, "newname")
	}

	all, err := app.FindAllRecords(players.CollectionName)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("expected exactly 1 players record, got %d", len(all))
	}
}

// TestEnsureMediaHonoursApprovalPerVersion pins the one behaviour that still
// legitimately differs between the endpoints: v1 queues a newly discovered
// Media for manual review, v2 publishes it straight away. Both now share the
// same code path, so this is what keeps the frozen v1 semantics from drifting.
func TestEnsureMediaHonoursApprovalPerVersion(t *testing.T) {
	app := newTestApp(t)

	if _, _, err := ensureMedia(app, testMedia("1001"), "", testPlayer("v1agent"), false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureMedia(app, testMedia("1002"), "", testPlayer("v2agent"), true); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		mediaID string
		want    bool
	}{{"1001", false}, {"1002", true}} {
		record, err := app.FindFirstRecordByData("medias", "media_id", c.mediaID)
		if err != nil {
			t.Fatal(err)
		}
		if got := record.GetBool("approved"); got != c.want {
			t.Errorf("media %s approved = %v, want %v", c.mediaID, got, c.want)
		}
	}
}
