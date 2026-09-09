package verification

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"
	"github.com/pocketbase/pocketbase/tools/types"
)

const (
	// codePrefix is what the admin plugin looks for in COMM. Keep it in step
	// with the pattern in comm.go.
	codePrefix = "IPV-"

	codeLength = 10

	// codeAlphabet drops O/0/I/1/L. An admin reads these off a COMM line by
	// eye, and a code that cannot be transcribed is worse than a shorter one.
	// 31^10 is about 2^49, which is far more than a single-use 30 minute code
	// needs.
	codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

	// codeTTL is deliberately short. A code is only in the agent's hands for
	// as long as it takes to paste it into the plugin; the admin half of the
	// flow works off the record, not the clock.
	codeTTL = 30 * time.Minute
)

var (
	// ErrContestedNeedsProof is basic trying to take a name off another
	// account. Basic is self-asserted, so it must never be able to.
	ErrContestedNeedsProof = errors.New("only advanced or strong verification can claim a contested name")

	// ErrNoDowngrade is an agent asking to verify at a tier below the one they
	// already hold. Silently weakening a verification would be worse than
	// refusing.
	ErrNoDowngrade = errors.New("you are already verified at a higher level")

	// ErrCodeUnusable covers unknown, expired and already-used codes alike.
	// Callers must not tell those apart to the client - doing so turns the
	// claim route into an oracle for guessing codes.
	ErrCodeUnusable = errors.New("verification code is not usable")
)

// Mint returns the caller's live pending code for this tier, creating one only
// if they do not already have one.
//
// Reuse is not just convenience. PocketBase 0.39's rate limiting is per-IP,
// global and not bindable to a single route, so handing back the existing code
// is the only per-account throttle available here: an agent hammering the
// button gets the same row every time rather than a fresh one.
//
// contested means the agent has asserted the name is theirs even though
// another account holds it, and claimedUsername is the name in question -
// their own profile cannot hold it yet, so there is nowhere else to put it.
func Mint(app core.App, user *core.Record, tier Tier, contested bool, claimedUsername string) (*core.Record, error) {
	if contested && !tier.Proves() {
		return nil, ErrContestedNeedsProof
	}

	if Tier(user.GetString("verification")).Rank() > tier.Rank() {
		return nil, ErrNoDowngrade
	}

	existing, err := findPending(app, user.Id, tier, contested, claimedUsername)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	collection, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		return nil, err
	}

	record := core.NewRecord(collection)
	record.Set("code", codePrefix+security.RandomStringWithAlphabet(codeLength, codeAlphabet))
	record.Set("user", user.Id)
	record.Set("tier", string(tier))
	record.Set("status", string(StatusPending))
	record.Set("contested", contested)
	record.Set("claimed_username", claimedUsername)
	record.Set("expires_at", types.NowDateTime().Add(codeTTL))

	if err := app.Save(record); err != nil {
		return nil, err
	}

	return record, nil
}

// findPending looks for an unexpired code the caller can reuse. A pending code
// for a different tier, or for a different contested name, is not reusable -
// the answer would be wrong - so it is left alone to expire.
//
// The contested/claimed_username comparison is done in Go rather than in the
// filter because PocketBase treats an empty string parameter as unset, and the
// ordinary case has an empty claimed_username.
func findPending(app core.App, userID string, tier Tier, contested bool, claimedUsername string) (*core.Record, error) {
	records, err := app.FindRecordsByFilter(
		CollectionName,
		"user = {:user} && tier = {:tier} && status = {:status} && expires_at > {:now}",
		"",
		0,
		0,
		dbx.Params{
			"user":   userID,
			"tier":   string(tier),
			"status": string(StatusPending),
			"now":    types.NowDateTime(),
		},
	)
	if err != nil {
		return nil, err
	}

	for _, record := range records {
		if record.GetBool("contested") != contested {
			continue
		}
		if !strings.EqualFold(record.GetString("claimed_username"), claimedUsername) {
			continue
		}
		return record, nil
	}

	return nil, nil
}

// Consume moves a code from one status to the next, but only while it is
// unexpired and actually in the status the caller expects.
//
// One conditional UPDATE, exactly like campaigns.ClaimCampaign: a read, a
// check and a Save is three statements and two concurrent claims would both
// pass the check. Expiry is in the WHERE clause for the same reason - checking
// it beforehand leaves a window.
func Consume(app core.App, code string, from, to Status) (*core.Record, error) {
	result, err := app.DB().
		NewQuery("UPDATE " + CollectionName + " SET status = {:to} WHERE code = {:code} AND status = {:from} AND expires_at > {:now}").
		Bind(dbx.Params{
			"to":   string(to),
			"code": code,
			"from": string(from),
			"now":  types.NowDateTime(),
		}).
		Execute()
	if err != nil {
		return nil, err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, ErrCodeUnusable
	}

	record, err := app.FindFirstRecordByData(CollectionName, "code", code)
	if err != nil {
		return nil, fmt.Errorf("re-reading consumed code: %w", err)
	}

	return record, nil
}

// FindUsableCode returns the record for a code that is still pending and
// unexpired, without consuming it. The claim route needs it to run the
// identity check before committing the code to a status.
func FindUsableCode(app core.App, code string) (*core.Record, error) {
	record, err := app.FindFirstRecordByFilter(
		CollectionName,
		"code = {:code} && status = {:status} && expires_at > {:now}",
		dbx.Params{"code": code, "status": string(StatusPending), "now": types.NowDateTime()},
	)
	if err != nil || record == nil {
		return nil, ErrCodeUnusable
	}
	return record, nil
}
