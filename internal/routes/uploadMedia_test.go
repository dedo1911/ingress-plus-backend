package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	// Both unique in production: media_id is the natural key (Niantic's id) and
	// url_id is the public identifier in /media/<url_id>. They are what actually
	// stops two concurrent uploads of the same new Media creating two records.
	medias.AddIndex("idx_medias_media_id", true, "media_id", "")
	medias.AddIndex("idx_medias_url_id", true, "url_id", "")
	if err := app.Save(medias); err != nil {
		t.Fatalf("creating medias collection: %v", err)
	}

	uploads := core.NewBaseCollection("media_uploads")
	uploads.Fields.Add(&core.TextField{Name: "media_url_id"})
	uploads.Fields.Add(&core.TextField{Name: "uploader_ign"})
	uploads.Fields.Add(&core.TextField{Name: "uploader_faction"})
	uploads.Fields.Add(&core.RelationField{Name: "player", CollectionId: playersCollection.Id, MaxSelect: 1})
	uploads.AddIndex("idx_media_uploads_unique", true, "media_url_id, uploader_ign", "")
	// Partial on purpose: PocketBase stores an unset relation as '', not NULL,
	// and roughly half of production has no identity - a plain unique index over
	// those would collide on the first two unattributed rows.
	uploads.AddIndex("idx_media_uploads_player", true, "media_url_id, player", "player != ''")
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

// TestEnsureKeepsKnownNicknameWhenGivenNone protects the backfill, which
// creates records for player IDs it deliberately refuses to attribute and has
// no trustworthy nickname to offer for them. Passing an empty nickname must
// leave a known one alone rather than wipe it.
func TestEnsureKeepsKnownNicknameWhenGivenNone(t *testing.T) {
	app := newTestApp(t)

	hasher, err := players.NewHasher("test-pepper")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hasher.Hash(testPlayerID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := players.Ensure(app, hash, "oscarc1", "RESISTANCE"); err != nil {
		t.Fatal(err)
	}

	record, err := players.Ensure(app, hash, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := record.GetString("last_ign"); got != "oscarc1" {
		t.Fatalf("last_ign = %q, want the known nickname to survive", got)
	}
	if got := record.GetString("last_faction"); got != "RESISTANCE" {
		t.Fatalf("last_faction = %q, want the known faction to survive", got)
	}
}

// The mirror case: a record created bare by the backfill picks up a nickname
// the first time its owner uploads again. That is the whole point of keeping
// the unattributed hashes.
func TestEnsureFillsInNicknameOnLaterUpload(t *testing.T) {
	app := newTestApp(t)

	hasher, err := players.NewHasher("test-pepper")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hasher.Hash(testPlayerID)
	if err != nil {
		t.Fatal(err)
	}

	bare, err := players.Ensure(app, hash, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := bare.GetString("last_ign"); got != "" {
		t.Fatalf("expected a bare record, got last_ign %q", got)
	}

	filled, err := players.Ensure(app, hash, "oscarc1", "RESISTANCE")
	if err != nil {
		t.Fatal(err)
	}
	if filled.Id != bare.Id {
		t.Fatal("a later upload created a second record instead of reusing the hashed one")
	}
	if got := filled.GetString("last_ign"); got != "oscarc1" {
		t.Fatalf("last_ign = %q, want it filled in from the upload", got)
	}
}

// TestEnsureMediaConcurrentDiscovery reproduces the incident that made the
// unique index impossible to add: several uploads of the same new Media
// arriving together all passed the existence check, because it runs outside the
// transaction, and each inserted its own record. Production ended up with 7
// duplicated media_ids, 2 of which also shared a url_id.
//
// Exactly one caller must create the Media; the rest must quietly agree it
// already exists and report the same url_id, so their upload is still logged.
func TestEnsureMediaConcurrentDiscovery(t *testing.T) {
	app := newTestApp(t)
	media := testMedia("4962")

	const callers = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	urlIDs := map[int]bool{}
	var failures []error

	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			urlID, wasCreated, err := ensureMedia(app, media, "", testPlayer("hisname"), true)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			if wasCreated {
				created++
			}
			urlIDs[urlID] = true
		}()
	}
	wg.Wait()

	for _, err := range failures {
		t.Errorf("a concurrent caller failed instead of recovering: %v", err)
	}
	if created != 1 {
		t.Errorf("created = %d, want exactly 1 discoverer", created)
	}
	if len(urlIDs) != 1 {
		t.Errorf("callers saw %d different url_ids, want 1: %v", len(urlIDs), urlIDs)
	}

	records, err := app.FindAllRecords("medias")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 medias record, got %d", len(records))
	}
}

// TestCreateMediaRecoversFromLostRace drives the recovery path directly by
// calling createMedia for a media_id that already exists - exactly the state a
// caller lands in when another upload wins the race between ensureMedia's
// existence check and the insert. The unique index rejects the insert, and the
// caller must resolve to the existing record instead of failing.
//
// Driving it this way rather than with goroutines is deliberate: SQLite
// serialises writers, so a concurrent test never actually interleaves at the
// point that matters and passes whether or not the recovery exists.
func TestCreateMediaRecoversFromLostRace(t *testing.T) {
	app := newTestApp(t)
	media := testMedia("4962")

	winnerURLID, created, err := ensureMedia(app, media, "", testPlayer("dai02"), true)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected the first caller to create the Media")
	}

	// the loser reaches the insert believing the Media is new
	urlID, created, err := createMedia(app, media, "", testPlayer("hisname"), true)
	if err != nil {
		t.Fatalf("losing the race returned an error instead of resolving: %v", err)
	}
	if created {
		t.Fatal("the loser reported creating a Media that already existed")
	}
	if urlID != winnerURLID {
		t.Fatalf("loser got url_id %d, want the winner's %d", urlID, winnerURLID)
	}

	records, err := app.FindAllRecords("medias")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 medias record, got %d - the duplicate was created anyway", len(records))
	}
}

// TestUploadMediaV1IsRetired pins the one thing the retired endpoint still has
// to do: tell an agent on an out-of-date plugin why their upload failed. Every
// plugin release from 1.0.2 onwards appends the response body to the alert it
// shows, so this text is what they actually read.
func TestUploadMediaV1IsRetired(t *testing.T) {
	app := newTestApp(t)

	e := &core.RequestEvent{App: app}
	e.Request = httptest.NewRequest("POST", "/api/mediagress/v1/upload-media", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	e.Response = rec

	if err := UploadMediaV1()(e); err != nil {
		t.Fatalf("retired handler returned an error: %v", err)
	}

	if rec.Code != http.StatusGone {
		t.Errorf("status = %d, want %d (Gone)", rec.Code, http.StatusGone)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "https://ingress.plus/media/upload") {
		t.Errorf("response does not tell the agent where to get the new plugin: %q", body)
	}
	if strings.Contains(body, "\n") {
		t.Errorf("response spans multiple lines; it is rendered inside an alert box: %q", body)
	}

	// nothing may be written - a retired endpoint must not half-record an upload
	for _, collection := range []string{"medias", "media_uploads", "players"} {
		records, err := app.FindAllRecords(collection)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 0 {
			t.Errorf("%s got %d records from a retired endpoint, want 0", collection, len(records))
		}
	}
}

// newTestPlayerRecord creates a players record for a distinct player ID, so a
// test can act as two different agents.
func newTestPlayerRecord(t *testing.T, app core.App, rawID, ign string) *core.Record {
	t.Helper()

	hasher, err := players.NewHasher("test-pepper")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hasher.Hash(rawID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := players.Ensure(app, hash, ign, "RESISTANCE")
	if err != nil {
		t.Fatal(err)
	}

	return record
}

// TestEnsureUploadDedupesAcrossRename is the point of keying on the player
// record: an agent who changes their username must not acquire a second row for
// a Media they had already uploaded, which would double-count them in the
// totals and split them across two names on the leaderboards.
func TestEnsureUploadDedupesAcrossRename(t *testing.T) {
	app := newTestApp(t)

	record := newTestPlayerRecord(t, app, testPlayerID, "oldname")

	if _, err := ensureUpload(app, 1, record.Id, testPlayer("oldname")); err != nil {
		t.Fatal(err)
	}

	firstTime, err := ensureUpload(app, 1, record.Id, testPlayer("newname"))
	if err != nil {
		t.Fatal(err)
	}
	if firstTime {
		t.Fatal("the same player under a new nickname counted as a first upload")
	}

	rows, err := app.FindAllRecords("media_uploads")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("a rename produced %d rows for one Media, want 1", len(rows))
	}
	// The name at upload time is the historical record and stays put;
	// players.last_ign is where the current name lives.
	if got := rows[0].GetString("uploader_ign"); got != "oldname" {
		t.Fatalf("the existing row's nickname was rewritten: got %q, want %q", got, "oldname")
	}
}

// TestClaimHistoryLinksEarlierUploads covers the coverage-growth path: an
// upload proves who a nickname belongs to, so every earlier row under that name
// can be attributed, not just the row being written now.
func TestClaimHistoryLinksEarlierUploads(t *testing.T) {
	app := newTestApp(t)

	// history from before the agent was ever identified
	for _, urlID := range []int{1, 2, 3} {
		if _, err := ensureUpload(app, urlID, "", testPlayer("oscarc1")); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := ensureMedia(app, testMedia("5000"), "", testPlayer("oscarc1"), true); err != nil {
		t.Fatal(err)
	}

	record := newTestPlayerRecord(t, app, testPlayerID, "oscarc1")

	result, err := players.ClaimHistory(app, record.Id, "oscarc1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Conflict {
		t.Fatal("claiming an unattributed nickname was reported as a conflict")
	}
	if result.Linked != 4 {
		t.Fatalf("linked %d rows, want 4 (3 uploads + 1 media)", result.Linked)
	}

	rows, err := app.FindAllRecords("media_uploads")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if got := row.GetString("player"); got != record.Id {
			t.Fatalf("an earlier upload was left unlinked: got %q, want %q", got, record.Id)
		}
	}
}

// TestClaimHistoryRefusesAnotherPlayersName is the guard that makes the sweep
// safe on an unauthenticated route. The nickname in an upload is self-asserted,
// so without this anyone with C.O.R.E. could post a single media under a
// well-known agent's name and take over their entire upload history.
func TestClaimHistoryRefusesAnotherPlayersName(t *testing.T) {
	app := newTestApp(t)

	owner := newTestPlayerRecord(t, app, testPlayerID, "dai02")
	if _, err := ensureUpload(app, 1, owner.Id, testPlayer("dai02")); err != nil {
		t.Fatal(err)
	}
	// an older row the owner has not been linked to yet
	if _, err := ensureUpload(app, 2, "", testPlayer("dai02")); err != nil {
		t.Fatal(err)
	}

	impostor := newTestPlayerRecord(t, app, "0edba40ac0ffee00deadbeef12345678.c", "impostor")

	result, err := players.ClaimHistory(app, impostor.Id, "dai02")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Conflict {
		t.Fatal("claiming a nickname owned by another player was not flagged as a conflict")
	}
	if result.Linked != 0 {
		t.Fatalf("a conflicting claim linked %d rows, want 0", result.Linked)
	}

	rows, err := app.FindAllRecords("media_uploads")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if got := row.GetString("player"); got == impostor.Id {
			t.Fatal("a row was handed to the impostor")
		}
	}
}

// TestClaimHistoryIgnoresScrubbedAttribution keeps the sweep away from the
// marker moderators leave when they strip an upload's attribution. It names no
// agent, so claiming everything under it would hand one player a pile of
// deliberately anonymised uploads.
func TestClaimHistoryIgnoresScrubbedAttribution(t *testing.T) {
	app := newTestApp(t)

	if _, err := ensureUpload(app, 1, "", testPlayer("UNKNOWN")); err != nil {
		t.Fatal(err)
	}

	record := newTestPlayerRecord(t, app, testPlayerID, "UNKNOWN")

	result, err := players.ClaimHistory(app, record.Id, "UNKNOWN")
	if err != nil {
		t.Fatal(err)
	}
	if result.Linked != 0 {
		t.Fatalf("claimed %d scrubbed rows, want 0", result.Linked)
	}
}
