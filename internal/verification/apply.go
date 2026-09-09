package verification

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"
)

// ErrContested is another account holding the name at an equal or higher tier.
// Equal tiers are not evidence of fraud - two agents can both be wrong, or one
// can have renamed - so nothing is touched and a human decides.
var ErrContested = errors.New("that agent name is already verified by another account at the same or a higher level")

// ErrWrongStatus is a verification record that is not at the point where it
// can be applied: already applied, rejected, or still waiting on COMM.
var ErrWrongStatus = errors.New("verification is not ready to be applied")

// placeholderPattern is what users.username accepts: letters and digits, at
// most 15 of them.
var placeholderPattern = regexp.MustCompile(`^[A-Za-z0-9]{3,15}$`)

// Trumped is an account that lost its name to a stronger claim.
type Trumped struct {
	UserID        string
	Email         string
	OldUsername   string
	FreedUsername string
}

// Result is what was established, for the caller to act on: the route uses it
// to drive Mediagress attribution and to notify anyone who was displaced.
type Result struct {
	Tier       Tier
	UserID     string
	Username   string
	Faction    string
	PlayerHash string
	Trumped    []Trumped
}

// Apply writes the verification onto the user record.
//
// Identity only. It does not touch players, or anything else that consumes a
// verification - see the package comment. The two are deliberately not atomic
// with each other: a verified user with no players link is already an ordinary
// state in this system (it is what every advanced verification looks like
// before the agent's first upload), so there is nothing to gain from dragging
// Mediagress into this transaction.
func Apply(app core.App, verificationID string) (Result, error) {
	var result Result

	err := app.RunInTransaction(func(txApp core.App) error {
		v, err := txApp.FindRecordById(CollectionName, verificationID)
		if err != nil {
			return err
		}

		tier, err := ParseTier(v.GetString("tier"))
		if err != nil {
			return err
		}

		// Basic is applied the moment the plugin calls in; the others wait for
		// an admin to read the code out of COMM.
		//
		// This check has to be inside the transaction. Consume stops two
		// requests claiming one code, but it cannot stop two Apply calls
		// racing on a record that was already consumed - this is what does.
		want := StatusClaimed
		if tier.Proves() {
			want = StatusConfirmed
		}
		if Status(v.GetString("status")) != want {
			return fmt.Errorf("%w: status is %q, expected %q", ErrWrongStatus, v.GetString("status"), want)
		}

		user, err := txApp.FindRecordById("users", v.GetString("user"))
		if err != nil {
			return err
		}

		if Tier(user.GetString("verification")).Rank() > tier.Rank() {
			return ErrNoDowngrade
		}

		nickname := v.GetString("nickname")
		faction := v.GetString("faction")

		// Re-checked here rather than trusted from claim time, because for
		// advanced and strong these are now the values an admin read off the
		// COMM plext, which may differ from what the plugin reported.
		if err := CheckIdentity(user, v, nickname, faction); err != nil {
			return err
		}

		trumped, err := takeUsername(txApp, user, nickname, tier, v.GetBool("contested"))
		if err != nil {
			return err
		}

		user.Set("verification", string(tier))
		if err := txApp.Save(user); err != nil {
			return err
		}

		v.Set("status", string(StatusApplied))
		if err := txApp.Save(v); err != nil {
			return err
		}

		result = Result{
			Tier:       tier,
			UserID:     user.Id,
			Username:   user.GetString("username"),
			Faction:    user.GetString("faction"),
			PlayerHash: v.GetString("player_hash"),
			Trumped:    trumped,
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}

	return result, nil
}

// takeUsername makes sure the user holds the name they just proved.
//
// In the ordinary case it does nothing at all: CheckIdentity has already
// established that the profile matches, so there is no username to write. The
// work below only happens on a contested claim, where the agent could not set
// the name themselves because another account holds it and usernames are
// unique.
func takeUsername(txApp core.App, user *core.Record, nickname string, tier Tier, contested bool) ([]Trumped, error) {
	if strings.EqualFold(user.GetString("username"), nickname) {
		return nil, nil
	}

	// CheckIdentity only lets a mismatch through for a contested claim at a
	// tier that proves something. Belt and braces: this is the one code path
	// that renames somebody else's account.
	if !contested || !tier.Proves() {
		return nil, &MismatchError{Field: "agent name", Reported: nickname, OnSite: user.GetString("username")}
	}

	holders, err := txApp.FindAllRecords("users",
		dbx.NewExp("username = {:name} COLLATE NOCASE", dbx.Params{"name": nickname}),
		dbx.NewExp("id != {:id}", dbx.Params{"id": user.Id}),
	)
	if err != nil {
		return nil, err
	}

	// Production's unique index on username is already COLLATE NOCASE, so this
	// is at most one row there. It is queried as a set because PocketBase's own
	// test fixture indexes username binary, where "Foo" and "foo" coexist.
	for _, holder := range holders {
		if Tier(holder.GetString("verification")).Rank() >= tier.Rank() {
			return nil, ErrContested
		}
	}

	trumped := make([]Trumped, 0, len(holders))
	for _, holder := range holders {
		freed, err := placeholderUsername(txApp, holder)
		if err != nil {
			return nil, err
		}

		entry := Trumped{
			UserID:      holder.Id,
			Email:       holder.Email(),
			OldUsername: holder.GetString("username"),
		}

		holder.Set("verification", "")
		holder.Set("username", freed)
		if err := txApp.Save(holder); err != nil {
			return nil, fmt.Errorf("freeing username %q: %w", entry.OldUsername, err)
		}

		entry.FreedUsername = freed
		trumped = append(trumped, entry)
	}

	// Staged, not written: Apply saves the user after this returns, which is
	// what keeps the ordering safe. SQLite enforces unique indexes per
	// statement rather than at commit, so a save carrying this name while a
	// holder above still holds it would fail on the spot with a bare
	// constraint error.
	//
	// So: do not add a txApp.Save(user) to this function. The separation is
	// the only thing guaranteeing every holder is freed first.
	user.Set("username", nickname)

	return trumped, nil
}

// placeholderUsername is the name a displaced account is moved to.
//
// Their own record id: exactly 15 lowercase alphanumerics, so it satisfies the
// field's length and character rules by construction and is unique because
// record ids are. It is also the string already shown to them on their own
// settings page as ING+<id>, so a displaced agent recognizes it and support can
// look them up from it without translation.
func placeholderUsername(txApp core.App, holder *core.Record) (string, error) {
	candidates := []string{holder.Id}
	for i := 0; i < 3; i++ {
		candidates = append(candidates, "Agent"+security.RandomStringWithAlphabet(codeLength, codeAlphabet))
	}

	for _, candidate := range candidates {
		if !placeholderPattern.MatchString(candidate) {
			continue
		}

		taken, err := txApp.FindAllRecords("users",
			dbx.NewExp("username = {:name} COLLATE NOCASE", dbx.Params{"name": candidate}),
		)
		if err != nil {
			return "", err
		}
		if len(taken) == 0 {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("could not find a free placeholder username for %s", holder.Id)
}
