package verification

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/security"
	"github.com/pocketbase/pocketbase/tools/types"
)

// newTestApp builds the two collections this package touches. PocketBase's own
// fixture already ships a users auth collection with a username, so only the
// two site-specific fields have to be added to it.
//
// Note that the fixture indexes username *binary* while production indexes it
// COLLATE NOCASE. That difference is deliberate here rather than papered over:
// it is the only way to exercise the case where more than one account holds a
// name, which the COLLATE NOCASE queries in apply.go have to cope with.
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
	users.Fields.Add(&core.SelectField{Name: "faction", MaxSelect: 1, Values: []string{"enlightened", "resistance", "machina"}})
	users.Fields.Add(&core.SelectField{Name: "verification", MaxSelect: 1, Values: []string{"basic", "advanced", "strong"}})
	if err := app.Save(users); err != nil {
		t.Fatalf("extending the users collection: %v", err)
	}

	v := core.NewBaseCollection(CollectionName)
	v.Fields.Add(&core.TextField{Name: "code"})
	v.Fields.Add(&core.RelationField{Name: "user", CollectionId: users.Id, MaxSelect: 1})
	v.Fields.Add(&core.SelectField{Name: "tier", MaxSelect: 1, Values: []string{"basic", "advanced", "strong"}})
	v.Fields.Add(&core.SelectField{Name: "status", MaxSelect: 1, Values: []string{"pending", "claimed", "confirmed", "applied", "rejected"}})
	v.Fields.Add(&core.BoolField{Name: "contested"})
	v.Fields.Add(&core.TextField{Name: "claimed_username"})
	v.Fields.Add(&core.DateField{Name: "expires_at"})
	v.Fields.Add(&core.TextField{Name: "nickname"})
	v.Fields.Add(&core.TextField{Name: "faction"})
	v.Fields.Add(&core.TextField{Name: "player_hash", Hidden: true})
	v.Fields.Add(&core.DateField{Name: "claimed_at"})
	v.Fields.Add(&core.DateField{Name: "confirmed_at"})
	v.Fields.Add(&core.TextField{Name: "note"})
	v.AddIndex("idx_agent_verifications_code", true, "code", "")
	if err := app.Save(v); err != nil {
		t.Fatalf("creating %s: %v", CollectionName, err)
	}

	return app
}

func newUser(t *testing.T, app core.App, username, faction, verification string) *core.Record {
	t.Helper()

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}

	record := core.NewRecord(users)
	// Usernames differing only by case are a real case here, so the email
	// cannot be derived from the username alone.
	record.SetEmail(security.PseudorandomString(10) + "@example.invalid")
	record.SetPassword("1234567890")
	record.Set("username", username)
	record.Set("faction", faction)
	record.Set("verification", verification)
	if err := app.Save(record); err != nil {
		t.Fatalf("creating user %q: %v", username, err)
	}

	return record
}

// stage drives a verification record to the point Apply expects, skipping the
// route layer: claimed for basic, confirmed for the tiers that go via COMM.
func stage(t *testing.T, app core.App, v *core.Record, nickname, faction string) *core.Record {
	t.Helper()

	status := StatusClaimed
	if Tier(v.GetString("tier")).Proves() {
		status = StatusConfirmed
	}

	v.Set("nickname", nickname)
	v.Set("faction", faction)
	v.Set("status", string(status))
	if err := app.Save(v); err != nil {
		t.Fatal(err)
	}

	return v
}

func TestTierRankOrdersTiersAndRejectsUnknowns(t *testing.T) {
	cases := map[Tier]int{
		TierBasic:    1,
		TierAdvanced: 2,
		TierStrong:   3,
		"":           0,
		"legendary":  0,
		"BASIC":      0,
	}

	for tier, want := range cases {
		if got := tier.Rank(); got != want {
			t.Errorf("Tier(%q).Rank() = %d, want %d", tier, got, want)
		}
	}

	// The point of ranking an unknown value 0: something that reached the
	// database without passing through ParseTier must never take a name.
	if Tier("legendary").Rank() >= TierBasic.Rank() {
		t.Fatal("an unrecognized tier outranked basic")
	}
	if TierBasic.Proves() {
		t.Fatal("basic must never count as proof")
	}
}

func TestMintReusesAPendingCode(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "oscarc1", "resistance", "")

	first, err := Mint(app, user, TierAdvanced, false, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Mint(app, user, TierAdvanced, false, "")
	if err != nil {
		t.Fatal(err)
	}

	if first.Id != second.Id {
		t.Fatal("minting twice created a second code; reuse is the only per-account throttle there is")
	}
	if !strings.HasPrefix(first.GetString("code"), codePrefix) {
		t.Fatalf("code %q is missing the %q prefix the admin plugin looks for", first.GetString("code"), codePrefix)
	}
	// Only the random part - the fixed prefix is allowed its own I.
	if random := strings.TrimPrefix(first.GetString("code"), codePrefix); strings.ContainsAny(random, "O0I1L") {
		t.Fatalf("code %q contains a character that cannot be transcribed from COMM by eye", first.GetString("code"))
	}
}

func TestMintIgnoresAnExpiredCode(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "oscarc1", "resistance", "")

	first, err := Mint(app, user, TierAdvanced, false, "")
	if err != nil {
		t.Fatal(err)
	}
	first.Set("expires_at", types.NowDateTime().Add(-time.Minute))
	if err := app.Save(first); err != nil {
		t.Fatal(err)
	}

	second, err := Mint(app, user, TierAdvanced, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Id == second.Id {
		t.Fatal("an expired code was handed back for reuse")
	}
}

func TestMintRefusesContestedBasic(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "someoneelse", "resistance", "")

	if _, err := Mint(app, user, TierBasic, true, "oscarc1"); !errors.Is(err, ErrContestedNeedsProof) {
		t.Fatalf("Mint(basic, contested) error = %v, want ErrContestedNeedsProof - basic proves nothing and must never take a name", err)
	}
}

func TestMintRefusesADowngrade(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "oscarc1", "resistance", "strong")

	if _, err := Mint(app, user, TierBasic, false, ""); !errors.Is(err, ErrNoDowngrade) {
		t.Fatalf("Mint below the held tier error = %v, want ErrNoDowngrade", err)
	}
}

// TestConsumeIsSingleUse is the replay guard at the code level. A read, a check
// and a Save would let every one of these goroutines through.
func TestConsumeIsSingleUse(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "oscarc1", "resistance", "")

	v, err := Mint(app, user, TierAdvanced, false, "")
	if err != nil {
		t.Fatal(err)
	}
	code := v.GetString("code")

	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Consume(app, code, StatusPending, StatusClaimed); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if won != 1 {
		t.Fatalf("%d concurrent claims succeeded on one code, want exactly 1", won)
	}
}

func TestConsumeRefusesAnExpiredCode(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "oscarc1", "resistance", "")

	v, err := Mint(app, user, TierAdvanced, false, "")
	if err != nil {
		t.Fatal(err)
	}
	v.Set("expires_at", types.NowDateTime().Add(-time.Second))
	if err := app.Save(v); err != nil {
		t.Fatal(err)
	}

	if _, err := Consume(app, v.GetString("code"), StatusPending, StatusClaimed); !errors.Is(err, ErrCodeUnusable) {
		t.Fatalf("Consume on an expired code error = %v, want ErrCodeUnusable", err)
	}
}

func TestCheckIdentityRequiresBothFields(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "oscarc1", "resistance", "")

	v, err := Mint(app, user, TierAdvanced, false, "")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		nickname string
		faction  string
		wantOK   bool
	}{
		{"exact match", "oscarc1", "resistance", true},
		{"case differs on both", "OSCARC1", "RESISTANCE", true},
		{"wrong faction", "oscarc1", "ENLIGHTENED", false},
		{"wrong nickname", "someoneelse", "RESISTANCE", false},
		{"empty nickname", "", "RESISTANCE", false},
		{"empty faction", "oscarc1", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckIdentity(user, v, c.nickname, c.faction)
			if c.wantOK && err != nil {
				t.Fatalf("CheckIdentity() = %v, want nil", err)
			}
			if !c.wantOK && err == nil {
				t.Fatal("CheckIdentity() accepted a profile that does not match")
			}
			// The agent reads this out of an alert box, so it has to name
			// what to change.
			if err != nil && !strings.Contains(err.Error(), "ingress.plus/verify") {
				t.Fatalf("mismatch message gives the agent nowhere to go: %q", err)
			}
		})
	}
}

// A contested claim is the one case where the profile deliberately does not
// hold the name, so the name typed on /verify stands in for it.
func TestCheckIdentityUsesClaimedUsernameWhenContested(t *testing.T) {
	app := newTestApp(t)
	newUser(t, app, "oscarc1", "resistance", "basic")
	claimant := newUser(t, app, "realagent", "resistance", "")

	v, err := Mint(app, claimant, TierAdvanced, true, "oscarc1")
	if err != nil {
		t.Fatal(err)
	}

	if err := CheckIdentity(claimant, v, "oscarc1", "RESISTANCE"); err != nil {
		t.Fatalf("contested claim for the name on the record was rejected: %v", err)
	}
	// Contested widens the username check, never the faction one.
	if err := CheckIdentity(claimant, v, "oscarc1", "ENLIGHTENED"); err == nil {
		t.Fatal("contested claim skipped the faction check")
	}
	// And it does not become a licence to claim any name at all.
	if err := CheckIdentity(claimant, v, "somebodyelse", "RESISTANCE"); err == nil {
		t.Fatal("contested claim accepted a name other than the one on the record")
	}
}

func TestApplyBasicSetsTheTierAndWritesNoUsername(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "oscarc1", "resistance", "")

	v, err := Mint(app, user, TierBasic, false, "")
	if err != nil {
		t.Fatal(err)
	}
	stage(t, app, v, "OSCARC1", "RESISTANCE")

	result, err := Apply(app, v.Id)
	if err != nil {
		t.Fatal(err)
	}
	if result.Tier != TierBasic {
		t.Fatalf("tier = %q, want basic", result.Tier)
	}
	if len(result.Trumped) != 0 {
		t.Fatal("basic displaced an account")
	}

	fresh, err := app.FindRecordById("users", user.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got := fresh.GetString("verification"); got != "basic" {
		t.Fatalf("verification = %q, want basic", got)
	}
	// Casing came back differently from Ingress, but basic is self-asserted
	// and must not rewrite the profile on the strength of it.
	if got := fresh.GetString("username"); got != "oscarc1" {
		t.Fatalf("username = %q, want it left alone at oscarc1", got)
	}
}

func TestApplyRefusesToWeakenAnExistingVerification(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "oscarc1", "resistance", "")

	v, err := Mint(app, user, TierBasic, false, "")
	if err != nil {
		t.Fatal(err)
	}
	stage(t, app, v, "oscarc1", "resistance")

	// Promoted by some other route between minting and applying.
	user.Set("verification", "strong")
	if err := app.Save(user); err != nil {
		t.Fatal(err)
	}

	if _, err := Apply(app, v.Id); !errors.Is(err, ErrNoDowngrade) {
		t.Fatalf("Apply() error = %v, want ErrNoDowngrade", err)
	}
}

// TestApplyTrumpsALowerTier covers the whole contested path end to end: the
// squatter is freed and the claimant actually ends up holding the name, which
// is the only outcome that matters to either of them.
//
// It does not pin the save ordering inside takeUsername - that is structural
// (takeUsername stages the name, Apply saves it afterwards) rather than
// something this can provoke.
func TestApplyTrumpsALowerTier(t *testing.T) {
	app := newTestApp(t)
	squatter := newUser(t, app, "oscarc1", "enlightened", "basic")
	claimant := newUser(t, app, "realagent", "resistance", "")

	v, err := Mint(app, claimant, TierAdvanced, true, "oscarc1")
	if err != nil {
		t.Fatal(err)
	}
	stage(t, app, v, "oscarc1", "RESISTANCE")

	result, err := Apply(app, v.Id)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Trumped) != 1 {
		t.Fatalf("displaced %d accounts, want 1", len(result.Trumped))
	}
	if result.Trumped[0].OldUsername != "oscarc1" {
		t.Fatalf("displaced the wrong account: %+v", result.Trumped[0])
	}

	freshClaimant, err := app.FindRecordById("users", claimant.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got := freshClaimant.GetString("username"); got != "oscarc1" {
		t.Fatalf("claimant username = %q, want oscarc1 - taking the name is the entire point", got)
	}
	if got := freshClaimant.GetString("verification"); got != "advanced" {
		t.Fatalf("claimant verification = %q, want advanced", got)
	}

	freshSquatter, err := app.FindRecordById("users", squatter.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got := freshSquatter.GetString("verification"); got != "" {
		t.Fatalf("squatter kept verification %q", got)
	}
	if got := freshSquatter.GetString("username"); got != squatter.Id {
		t.Fatalf("squatter username = %q, want their record id %q", got, squatter.Id)
	}
}

func TestApplyRefusesToTrumpAnEqualOrHigherTier(t *testing.T) {
	app := newTestApp(t)
	holder := newUser(t, app, "oscarc1", "enlightened", "advanced")
	claimant := newUser(t, app, "realagent", "resistance", "")

	v, err := Mint(app, claimant, TierAdvanced, true, "oscarc1")
	if err != nil {
		t.Fatal(err)
	}
	stage(t, app, v, "oscarc1", "RESISTANCE")

	if _, err := Apply(app, v.Id); !errors.Is(err, ErrContested) {
		t.Fatalf("Apply() error = %v, want ErrContested", err)
	}

	// Everything must be exactly as it was - this is what proves the whole
	// transaction rolled back rather than half-applying.
	freshHolder, err := app.FindRecordById("users", holder.Id)
	if err != nil {
		t.Fatal(err)
	}
	if freshHolder.GetString("username") != "oscarc1" || freshHolder.GetString("verification") != "advanced" {
		t.Fatalf("holder was modified by a refused claim: %s / %s",
			freshHolder.GetString("username"), freshHolder.GetString("verification"))
	}

	freshClaimant, err := app.FindRecordById("users", claimant.Id)
	if err != nil {
		t.Fatal(err)
	}
	if freshClaimant.GetString("verification") != "" {
		t.Fatal("claimant was verified despite the refusal")
	}
	if freshClaimant.GetString("username") != "realagent" {
		t.Fatal("claimant was renamed despite the refusal")
	}
}

func TestApplyIsNotReplayable(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "oscarc1", "resistance", "")

	v, err := Mint(app, user, TierBasic, false, "")
	if err != nil {
		t.Fatal(err)
	}
	stage(t, app, v, "oscarc1", "resistance")

	if _, err := Apply(app, v.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(app, v.Id); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("second Apply() error = %v, want ErrWrongStatus", err)
	}
}

func TestApplyRefusesAMismatchedFaction(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "oscarc1", "resistance", "")

	v, err := Mint(app, user, TierAdvanced, false, "")
	if err != nil {
		t.Fatal(err)
	}
	// What COMM actually attested disagrees with the profile.
	stage(t, app, v, "oscarc1", "ENLIGHTENED")

	var mismatch *MismatchError
	if _, err := Apply(app, v.Id); !errors.As(err, &mismatch) {
		t.Fatalf("Apply() error = %v, want a MismatchError", err)
	}

	fresh, err := app.FindRecordById("users", user.Id)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.GetString("verification") != "" {
		t.Fatal("a mismatched faction still granted a tier")
	}
}

// The fixture indexes username binary, so two accounts can hold names that
// differ only by case. Production's COLLATE NOCASE index makes that impossible,
// but the queries have to cope either way.
func TestApplyTrumpsEveryCaseVariantOfTheName(t *testing.T) {
	app := newTestApp(t)
	lower := newUser(t, app, "oscarc1", "enlightened", "basic")
	upper := newUser(t, app, "OSCARC1", "enlightened", "")
	claimant := newUser(t, app, "realagent", "resistance", "")

	v, err := Mint(app, claimant, TierStrong, true, "oscarc1")
	if err != nil {
		t.Fatal(err)
	}
	stage(t, app, v, "Oscarc1", "RESISTANCE")

	result, err := Apply(app, v.Id)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Trumped) != 2 {
		t.Fatalf("displaced %d accounts, want both case variants", len(result.Trumped))
	}

	for _, id := range []string{lower.Id, upper.Id} {
		fresh, err := app.FindRecordById("users", id)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.GetString("username") != id {
			t.Fatalf("account %s was left holding %q", id, fresh.GetString("username"))
		}
	}
}

func TestPlaceholderUsernameFitsTheField(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "oscarc1", "resistance", "")

	got, err := placeholderUsername(app, user)
	if err != nil {
		t.Fatal(err)
	}
	if got != user.Id {
		t.Fatalf("placeholder = %q, want the record id %q", got, user.Id)
	}
	if !placeholderPattern.MatchString(got) {
		t.Fatalf("placeholder %q is not a username the field would accept", got)
	}
}

func TestPlaceholderUsernameFallsBackWhenTheIdIsTaken(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "oscarc1", "resistance", "")
	// Somebody typed the displaced account's record id as their own username.
	newUser(t, app, user.Id, "enlightened", "")

	got, err := placeholderUsername(app, user)
	if err != nil {
		t.Fatal(err)
	}
	if got == user.Id {
		t.Fatal("placeholder collided with an existing username")
	}
	if !placeholderPattern.MatchString(got) {
		t.Fatalf("fallback placeholder %q is not a username the field would accept", got)
	}
}

func TestCommDeadlineGivesTheAdminsAWeek(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "oscarc1", "resistance", "")

	v, err := Mint(app, user, TierAdvanced, false, "")
	if err != nil {
		t.Fatal(err)
	}

	// The 30 minute TTL bounds the agent pasting the code in. Once the plext is
	// in COMM the clock belongs to the admins, who read the feed whenever they
	// get to it - the claim route stamps this on the record.
	deadline := CommDeadline()
	if left := time.Until(deadline.Time()); left < 6*24*time.Hour || left > 8*24*time.Hour {
		t.Fatalf("CommDeadline is %v away, want about a week", left)
	}

	if _, err := Consume(app, v.GetString("code"), StatusPending, StatusClaimed); err != nil {
		t.Fatal(err)
	}
	v, err = app.FindFirstRecordByData(CollectionName, "code", v.GetString("code"))
	if err != nil {
		t.Fatal(err)
	}
	v.Set("expires_at", deadline)
	if err := app.Save(v); err != nil {
		t.Fatal(err)
	}

	confirmed, err := Consume(app, v.GetString("code"), StatusClaimed, StatusConfirmed)
	if err != nil {
		t.Fatalf("confirming inside the window: %v", err)
	}
	if Status(confirmed.GetString("status")) != StatusConfirmed {
		t.Fatalf("status = %q, want %q", confirmed.GetString("status"), StatusConfirmed)
	}
}

func TestConfirmingAfterTheWindowIsRefused(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "latecomer", "resistance", "")

	v, err := Mint(app, user, TierAdvanced, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Consume(app, v.GetString("code"), StatusPending, StatusClaimed); err != nil {
		t.Fatal(err)
	}

	v, err = app.FindFirstRecordByData(CollectionName, "code", v.GetString("code"))
	if err != nil {
		t.Fatal(err)
	}
	v.Set("expires_at", types.NowDateTime().Add(-time.Second))
	if err := app.Save(v); err != nil {
		t.Fatal(err)
	}

	// A verification nobody confirmed lapses rather than staying claimable
	// forever. The agent can mint a new code and post again.
	if _, err := Consume(app, v.GetString("code"), StatusClaimed, StatusConfirmed); !errors.Is(err, ErrCodeUnusable) {
		t.Fatalf("error = %v, want ErrCodeUnusable", err)
	}
}

func TestCommTargetIsWhatTheAdminToolingLooksFor(t *testing.T) {
	app := newTestApp(t)
	user := newUser(t, app, "oscarc1", "resistance", "")

	v, err := Mint(app, user, TierAdvanced, false, "")
	if err != nil {
		t.Fatal(err)
	}

	comm := CommTarget(v.GetString("code"))
	// A real plext as the admin plugin sees it: a nickname, then the message.
	plext := "<oscarc1> " + comm.Message

	if got := CommPattern.FindString(plext); got != comm.Message {
		t.Fatalf("CommPattern found %q in %q, want %q", got, plext, comm.Message)
	}
	if comm.LatE6 != -48876667 || comm.LngE6 != -123393333 {
		t.Fatalf("coordinates = %d,%d, want Point Nemo", comm.LatE6, comm.LngE6)
	}
}

func TestEnabledFailsClosed(t *testing.T) {
	app := newTestApp(t)

	// No feature_flags collection at all: the flag cannot be read, so the flow
	// is closed rather than open.
	if Enabled(app) {
		t.Fatal("Enabled reported true with no flag to read")
	}

	flags := core.NewBaseCollection("feature_flags")
	flags.Fields.Add(&core.TextField{Name: "name"})
	flags.Fields.Add(&core.BoolField{Name: "enabled"})
	if err := app.Save(flags); err != nil {
		t.Fatal(err)
	}
	if Enabled(app) {
		t.Fatal("Enabled reported true with no record")
	}

	record := core.NewRecord(flags)
	record.Set("name", FlagName)
	record.Set("enabled", true)
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}
	if !Enabled(app) {
		t.Fatal("Enabled reported false with the flag on")
	}
}
