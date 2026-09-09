package players

import (
	"errors"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/security"

	"github.com/dedo1911/ingress-plus-backend/internal/verification"
)

// newTestApp builds players on top of the fixture's users collection, with the
// partial unique index production has - the one that makes the ordering inside
// link() matter.
func newTestApp(t *testing.T) *tests.TestApp {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	users.Fields.Add(&core.SelectField{Name: "verification", MaxSelect: 1, Values: []string{"basic", "advanced", "strong"}})
	if err := app.Save(users); err != nil {
		t.Fatalf("extending the users collection: %v", err)
	}

	collection := core.NewBaseCollection(CollectionName)
	collection.Fields.Add(&core.TextField{Name: "player_hash"})
	collection.Fields.Add(&core.TextField{Name: "last_ign"})
	collection.Fields.Add(&core.TextField{Name: "last_faction"})
	collection.Fields.Add(&core.RelationField{Name: "user", CollectionId: users.Id, MaxSelect: 1})
	collection.Fields.Add(&core.DateField{Name: "verified_at"})
	collection.AddIndex("idx_players_hash", true, "player_hash", "")
	collection.AddIndex("idx_players_user", true, "user", "user != ''")
	if err := app.Save(collection); err != nil {
		t.Fatalf("creating %s: %v", CollectionName, err)
	}

	return app
}

func newUser(t *testing.T, app core.App, username, tier string) *core.Record {
	t.Helper()

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}

	record := core.NewRecord(users)
	record.SetEmail(security.PseudorandomString(10) + "@example.invalid")
	record.SetPassword("1234567890")
	record.Set("username", username)
	record.Set("verification", tier)
	if err := app.Save(record); err != nil {
		t.Fatalf("creating user %q: %v", username, err)
	}

	return record
}

func newPlayer(t *testing.T, app core.App, hash, ign, userID string) *core.Record {
	t.Helper()

	record, err := Ensure(app, hash, ign, "RESISTANCE")
	if err != nil {
		t.Fatal(err)
	}
	if userID != "" {
		record.Set("user", userID)
		if err := app.Save(record); err != nil {
			t.Fatal(err)
		}
	}

	return record
}

func reload(t *testing.T, app core.App, record *core.Record) *core.Record {
	t.Helper()

	fresh, err := app.FindRecordById(CollectionName, record.Id)
	if err != nil {
		t.Fatal(err)
	}
	return fresh
}

func TestApplyVerificationLinksAdvancedByNickname(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "oscarc1", "advanced")
	// Cased differently on purpose: the nickname on the row is whatever the
	// last upload reported, not what the site holds.
	player := newPlayer(t, app, "hash-a", "OscarC1", "")

	if err := ApplyVerification(app, verification.Result{
		Tier: verification.TierAdvanced, UserID: user.Id, Username: "oscarc1", Faction: "resistance",
	}); err != nil {
		t.Fatalf("ApplyVerification: %v", err)
	}

	linked := reload(t, app, player)
	if linked.GetString("user") != user.Id {
		t.Fatalf("user = %q, want %q", linked.GetString("user"), user.Id)
	}
	if linked.GetDateTime("verified_at").IsZero() {
		t.Fatal("verified_at was not stamped")
	}
}

func TestApplyVerificationAdvancedWithNoUploadsIsNotAnError(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "newcomer", "advanced")

	// The normal case for an agent who verifies before uploading. Nothing to
	// link, and LinkVerifiedUser catches them later.
	if err := ApplyVerification(app, verification.Result{
		Tier: verification.TierAdvanced, UserID: user.Id, Username: "newcomer",
	}); err != nil {
		t.Fatalf("ApplyVerification: %v", err)
	}
}

func TestApplyVerificationRefusesToGuessBetweenTwoPlayers(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "twin", "advanced")
	first := newPlayer(t, app, "hash-a", "twin", "")
	second := newPlayer(t, app, "hash-b", "TWIN", "")

	err := ApplyVerification(app, verification.Result{
		Tier: verification.TierAdvanced, UserID: user.Id, Username: "twin",
	})
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("error = %v, want ErrAmbiguous", err)
	}

	for _, player := range []*core.Record{first, second} {
		if got := reload(t, app, player).GetString("user"); got != "" {
			t.Fatalf("player %s was linked to %q", player.Id, got)
		}
	}
}

func TestApplyVerificationStrongCreatesTheRowFromTheHash(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "prover", "strong")

	if err := ApplyVerification(app, verification.Result{
		Tier: verification.TierStrong, UserID: user.Id, Username: "prover",
		Faction: "enlightened", PlayerHash: "hash-new",
	}); err != nil {
		t.Fatalf("ApplyVerification: %v", err)
	}

	record, err := app.FindFirstRecordByData(CollectionName, "player_hash", "hash-new")
	if err != nil {
		t.Fatal(err)
	}
	if record.GetString("user") != user.Id {
		t.Fatalf("user = %q, want %q", record.GetString("user"), user.Id)
	}
	// Uppercase, matching medias.uploader_faction rather than users.faction.
	if record.GetString("last_faction") != "ENLIGHTENED" {
		t.Fatalf("last_faction = %q, want ENLIGHTENED", record.GetString("last_faction"))
	}
}

func TestApplyVerificationBasicTouchesNothing(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "asserter", "basic")
	player := newPlayer(t, app, "hash-a", "asserter", "")

	if err := ApplyVerification(app, verification.Result{
		Tier: verification.TierBasic, UserID: user.Id, Username: "asserter",
	}); err != nil {
		t.Fatalf("ApplyVerification: %v", err)
	}

	if got := reload(t, app, player).GetString("user"); got != "" {
		t.Fatalf("basic linked a player record: %q", got)
	}
}

func TestApplyVerificationUnlinksTrumpedAccounts(t *testing.T) {
	app := newTestApp(t)
	squatter := newUser(t, app, "placeholder000", "")
	winner := newUser(t, app, "realagent", "advanced")
	stolen := newPlayer(t, app, "hash-a", "realagent", squatter.Id)

	if err := ApplyVerification(app, verification.Result{
		Tier: verification.TierAdvanced, UserID: winner.Id, Username: "realagent",
		Trumped: []verification.Trumped{{UserID: squatter.Id, OldUsername: "realagent"}},
	}); err != nil {
		t.Fatalf("ApplyVerification: %v", err)
	}

	if got := reload(t, app, stolen).GetString("user"); got != winner.Id {
		t.Fatalf("user = %q, want the winner %q", got, winner.Id)
	}
}

func TestApplyVerificationMovesTheUserOffTheirOldPlayerRow(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "renamed", "strong")
	old := newPlayer(t, app, "hash-old", "renamed", user.Id)

	// A second row on the same site user would violate the partial unique index
	// on players.user, so the old row has to be released first.
	if err := ApplyVerification(app, verification.Result{
		Tier: verification.TierStrong, UserID: user.Id, Username: "renamed", PlayerHash: "hash-new",
	}); err != nil {
		t.Fatalf("ApplyVerification: %v", err)
	}

	if got := reload(t, app, old).GetString("user"); got != "" {
		t.Fatalf("old player row still held by %q", got)
	}
	fresh, err := app.FindFirstRecordByData(CollectionName, "player_hash", "hash-new")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.GetString("user") != user.Id {
		t.Fatalf("new row user = %q, want %q", fresh.GetString("user"), user.Id)
	}
}

func TestApplyVerificationWillNotTakeAPlayerFromAnEqualTier(t *testing.T) {
	app := newTestApp(t)
	holder := newUser(t, app, "holder", "advanced")
	claimant := newUser(t, app, "claimant", "advanced")
	player := newPlayer(t, app, "hash-a", "claimant", holder.Id)

	err := ApplyVerification(app, verification.Result{
		Tier: verification.TierAdvanced, UserID: claimant.Id, Username: "claimant",
	})
	if !errors.Is(err, ErrHeldByStrongerAccount) {
		t.Fatalf("error = %v, want ErrHeldByStrongerAccount", err)
	}
	if got := reload(t, app, player).GetString("user"); got != holder.Id {
		t.Fatalf("user = %q, want the original holder %q", got, holder.Id)
	}
}

func TestUnlinkIsIdempotent(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "leaver", "advanced")
	player := newPlayer(t, app, "hash-a", "leaver", user.Id)

	for i := 0; i < 2; i++ {
		if err := Unlink(app, user.Id); err != nil {
			t.Fatalf("Unlink: %v", err)
		}
	}

	if got := reload(t, app, player).GetString("user"); got != "" {
		t.Fatalf("user = %q, want empty", got)
	}
}

func TestLinkVerifiedUserOnlyLinksProvenTiers(t *testing.T) {
	cases := map[string]bool{"": false, "basic": false, "advanced": true, "strong": true}

	for tier, wantLink := range cases {
		t.Run("tier="+tier, func(t *testing.T) {
			app := newTestApp(t)
			user := newUser(t, app, "uploader", tier)
			player := newPlayer(t, app, "hash-a", "UPLOADER", "")

			if err := LinkVerifiedUser(app, player); err != nil {
				t.Fatalf("LinkVerifiedUser: %v", err)
			}

			linked := reload(t, app, player).GetString("user") == user.Id
			if linked != wantLink {
				t.Fatalf("linked = %v, want %v", linked, wantLink)
			}
		})
	}
}

func TestLinkVerifiedUserLeavesAnAlreadyOwnedRowAlone(t *testing.T) {
	app := newTestApp(t)
	owner := newUser(t, app, "owner", "strong")
	newUser(t, app, "claimant", "advanced")
	player := newPlayer(t, app, "hash-a", "claimant", owner.Id)

	if err := LinkVerifiedUser(app, player); err != nil {
		t.Fatalf("LinkVerifiedUser: %v", err)
	}

	if got := reload(t, app, player).GetString("user"); got != owner.Id {
		t.Fatalf("user = %q, want the existing owner %q", got, owner.Id)
	}
}

func TestLinkVerifiedUserSkipsScrubbedNicknames(t *testing.T) {
	app := newTestApp(t)
	newUser(t, app, "UNKNOWN", "strong")
	player := newPlayer(t, app, "hash-a", "UNKNOWN", "")

	if err := LinkVerifiedUser(app, player); err != nil {
		t.Fatalf("LinkVerifiedUser: %v", err)
	}

	if got := reload(t, app, player).GetString("user"); got != "" {
		t.Fatalf("a scrubbed row was linked to %q", got)
	}
}

// unverifyRequest is the smallest event the hook needs, built the way the
// campaigns guard test builds one. Next() has no handlers bound, so it is a
// no-op: what is under test is the unlink, not PocketBase saving the user.
func unverifyRequest(t *testing.T, app core.App, record *core.Record, verification string) *core.RecordRequestEvent {
	t.Helper()

	// Reloaded so Original() holds the stored value, which is what the hook
	// compares against - a record built in memory has no original.
	fresh, err := app.FindRecordById("users", record.Id)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Set("verification", verification)

	e := &core.RecordRequestEvent{RequestEvent: &core.RequestEvent{}}
	e.App = app
	e.Record = fresh
	return e
}

func TestUnlinkOnUnverifyRequestReleasesTheIdentity(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "quitter", "strong")
	player := newPlayer(t, app, "hash-a", "quitter", user.Id)

	if err := UnlinkOnUnverifyRequest(unverifyRequest(t, app, user, "")); err != nil {
		t.Fatalf("UnlinkOnUnverifyRequest: %v", err)
	}

	if got := reload(t, app, player).GetString("user"); got != "" {
		t.Fatalf("user = %q, want empty", got)
	}
}

func TestUnlinkOnUnverifyRequestLeavesOtherEditsAlone(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "stayer", "advanced")
	player := newPlayer(t, app, "hash-a", "stayer", user.Id)

	// An upgrade, not an unverify: the identity stays put.
	if err := UnlinkOnUnverifyRequest(unverifyRequest(t, app, user, "strong")); err != nil {
		t.Fatalf("UnlinkOnUnverifyRequest: %v", err)
	}

	if got := reload(t, app, player).GetString("user"); got != user.Id {
		t.Fatalf("user = %q, want %q", got, user.Id)
	}
}
