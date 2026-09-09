package players

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/dedo1911/ingress-plus-backend/internal/verification"
)

// ErrAmbiguous is more than one "players" row answering to the verified
// nickname. Nothing is linked: never guess which Ingress account someone is.
var ErrAmbiguous = errors.New("more than one player record carries that nickname")

// ErrHeldByStrongerAccount is the Ingress identity already belonging to an
// account verified at least as strongly. Same rule as verification.Apply uses
// for a contested username, for the same reason: an equal tier is not evidence
// of fraud, so a human decides.
var ErrHeldByStrongerAccount = errors.New("that Ingress account is already claimed by an equally or better verified user")

// ApplyVerification points the agent's "players" record at the account that
// just proved it, and takes it away from anyone displaced.
//
// Advisory by design. This is Mediagress attribution, a consumer of the
// verification, not part of it - the caller logs a failure and still returns
// success, because an identity that has been proven stays proven whether or not
// there are uploads to hang off it. See the comment on verification.Apply.
func ApplyVerification(app core.App, result verification.Result) error {
	// Whoever lost their name loses the uploads posted under it with it.
	for _, trumped := range result.Trumped {
		if err := Unlink(app, trumped.UserID); err != nil {
			return fmt.Errorf("unlinking trumped user %s: %w", trumped.UserID, err)
		}
	}

	record, err := targetPlayer(app, result)
	if err != nil || record == nil {
		return err
	}

	return link(app, record, result.UserID, result.Tier)
}

// targetPlayer finds the Ingress account a verification proved, or nil if there
// is nothing to link.
func targetPlayer(app core.App, result verification.Result) (*core.Record, error) {
	switch result.Tier {
	case verification.TierStrong:
		// The hash is the identity, so this is the only tier that can create
		// the row: an agent who has never uploaded still has a player ID.
		if result.PlayerHash == "" {
			return nil, errors.New("strong verification carries no player hash")
		}
		// Uppercased to match what the upload route stores - last_faction
		// mirrors medias.uploader_faction (RESISTANCE), not users.faction.
		return Ensure(app, result.PlayerHash, result.Username, strings.ToUpper(result.Faction))

	case verification.TierAdvanced:
		// No hash, so the nickname is the only join key - and the row only
		// exists once the agent has uploaded. Nothing to link is the normal
		// case for an agent verifying first; LinkVerifiedUser catches them on
		// their next upload.
		matches, err := app.FindAllRecords(CollectionName,
			dbx.NewExp("last_ign = {:ign} COLLATE NOCASE", dbx.Params{"ign": result.Username}),
		)
		if err != nil {
			return nil, err
		}
		switch len(matches) {
		case 0:
			return nil, nil
		case 1:
			return matches[0], nil
		default:
			return nil, fmt.Errorf("%w: %d rows for %q", ErrAmbiguous, len(matches), result.Username)
		}

	default:
		// basic proves nothing, so it is a display badge and never touches
		// attribution.
		return nil, nil
	}
}

// link makes the user the owner of this Ingress identity.
func link(app core.App, record *core.Record, userID string, tier verification.Tier) error {
	holder := record.GetString("user")
	if holder == userID {
		return nil
	}

	if holder != "" {
		other, err := app.FindRecordById("users", holder)
		if err != nil {
			return err
		}
		if verification.Tier(other.GetString("verification")).Rank() >= tier.Rank() {
			return fmt.Errorf("%w: player %s is held by %s", ErrHeldByStrongerAccount, record.Id, holder)
		}
	}

	// Before, not after: production has a partial unique index on players.user,
	// and SQLite enforces it per statement rather than at commit, so a save
	// carrying this user while their old row still names them fails outright.
	if err := clearLinks(app, userID, record.Id); err != nil {
		return err
	}

	record.Set("user", userID)
	record.Set("verified_at", types.NowDateTime())
	return app.Save(record)
}

// Unlink drops whatever Ingress identity the user owns. Used when a
// verification is cleared and when a stronger claim takes the name away.
func Unlink(app core.App, userID string) error {
	return clearLinks(app, userID, "")
}

// clearLinks releases every row the user owns except the one they are about to
// be given.
//
// Raw SQL for the reason ClaimHistory gives: app.Save stamps "updated", and
// moderators triage records by that field.
func clearLinks(app core.App, userID, keepPlayerID string) error {
	if userID == "" {
		return nil
	}

	_, err := app.DB().
		NewQuery("UPDATE " + CollectionName + " SET user = '', verified_at = '' " +
			"WHERE user = {:user} AND id != {:keep}").
		Bind(dbx.Params{"user": userID, "keep": keepPlayerID}).
		Execute()
	if err != nil {
		return fmt.Errorf("releasing player records held by %s: %w", userID, err)
	}
	return nil
}

// LinkVerifiedUser attaches a "players" record to the verified account whose
// username matches the nickname on it, if there is exactly one.
//
// This is what makes advanced verification work at all. Advanced produces no
// hash, so an agent who verifies before their first upload has no row to link
// to - and given uploads run at roughly one a month, waiting for them to verify
// again would mean never. Instead the live upload path retries it every time.
//
// Live upload path only. The backfill calls Ensure too and must not mass-link
// on a username match, which is exactly the self-asserted value verification
// exists to stop trusting.
func LinkVerifiedUser(app core.App, record *core.Record) error {
	if record.GetString("user") != "" {
		return nil
	}

	ign := record.GetString("last_ign")
	if ign == "" || ign == "UNKNOWN" {
		return nil
	}

	// COLLATE NOCASE because production's username index is, so this is at most
	// one row there; handled as a set because the test fixture indexes binary.
	matches, err := app.FindAllRecords("users",
		dbx.NewExp("username = {:ign} COLLATE NOCASE", dbx.Params{"ign": ign}),
	)
	if err != nil {
		return err
	}
	if len(matches) != 1 {
		return nil
	}

	tier := verification.Tier(matches[0].GetString("verification"))
	if !tier.Proves() {
		return nil
	}

	return link(app, record, matches[0].Id, tier)
}

// UnlinkOnUnverifyRequest releases an agent's Ingress identity when their
// verification is cleared. users.updateRule lets an agent do that to
// themselves, and an unverified account must not keep owning a players record.
// Register on app.OnRecordUpdateRequest("users").
//
// OnRecordUpdateRequest rather than OnRecordAfterUpdateSuccess: this is
// request-scoped, so Go-side saves - including the ones the verification flow
// makes itself - do not re-enter it, while an admin UI edit does.
//
// The work goes after e.Next() because the old value is only meaningful once
// the new one has actually been written. Advisory: the response is already on
// its way, so a failure is logged rather than returned.
//
// Account deletion needs no equivalent - players.user is non-cascading, so
// PocketBase clears it.
func UnlinkOnUnverifyRequest(e *core.RecordRequestEvent) error {
	was := e.Record.Original().GetString("verification")

	if err := e.Next(); err != nil {
		return err
	}

	if was == "" || e.Record.GetString("verification") != "" {
		return nil
	}

	if err := Unlink(e.App, e.Record.Id); err != nil {
		e.App.Logger().Error("Failed to release the player record of an unverified account",
			"user", e.Record.Id, "error", err)
	}

	return nil
}
