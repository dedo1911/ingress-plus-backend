// Package verification establishes that an Ingress Plus account's username and
// faction really are a given Ingress agent's, at a stated level of proof.
//
// It is a general site feature, not a Mediagress one. What it produces is a
// value in users.verification; who consumes that - bug report screenshots
// today, Mediagress upload attribution, whatever comes later - is not this
// package's concern. That is why nothing here imports internal/players: the
// route wires the two together, and the two are deliberately not atomic with
// each other (see Apply).
package verification

import "fmt"

// CollectionName holds one record per verification attempt.
const CollectionName = "agent_verifications"

// Tier is how strongly an agent's identity has been proven.
type Tier string

const (
	// TierBasic is the plugin reporting the agent's own nickname and faction
	// from a page they control. It proves nothing - see Rank - and exists so
	// an agent can put a name to their account without an admin.
	TierBasic Tier = "basic"

	// TierAdvanced is a code posted to Ingress COMM and read back by an admin.
	// Niantic attests both the nickname and the faction on the plext, so this
	// is the first tier carrying real evidence. No C.O.R.E. needed.
	TierAdvanced Tier = "advanced"

	// TierStrong is TierAdvanced plus the player ID read out of the agent's
	// own inventory, which binds the peppered hash to the account. Requires a
	// C.O.R.E. subscription, same as uploading.
	TierStrong Tier = "strong"
)

// Rank orders the tiers so they can be compared. Anything unrecognized - and
// the empty string, meaning unverified - ranks 0.
//
// Comparisons are always on Rank and never on the string, so a value that
// somehow reaches the database without passing through here can never outrank
// a real tier, and so can never take a name off anyone.
func (t Tier) Rank() int {
	switch t {
	case TierBasic:
		return 1
	case TierAdvanced:
		return 2
	case TierStrong:
		return 3
	default:
		return 0
	}
}

// Proves reports whether the tier carries evidence beyond the claimant's own
// assertion. Only these tiers may take a contested name or be attributed
// uploads.
func (t Tier) Proves() bool {
	return t == TierAdvanced || t == TierStrong
}

// ParseTier accepts only the three known tiers. The empty string is rejected:
// it means "unverified", which is a state rather than something to verify at.
func ParseTier(raw string) (Tier, error) {
	tier := Tier(raw)
	if tier.Rank() == 0 {
		return "", fmt.Errorf("%q is not a verification tier", raw)
	}
	return tier, nil
}

// Status is where a verification attempt has got to.
type Status string

const (
	// StatusPending is minted but not yet used. A failed identity check leaves
	// the record here on purpose, so an agent who fixes their profile can
	// retry with the same code.
	StatusPending Status = "pending"

	// StatusClaimed means the plugin called in and the identity check passed.
	// Terminal for basic, which is applied immediately.
	StatusClaimed Status = "claimed"

	// StatusConfirmed means an admin read the code out of COMM.
	StatusConfirmed Status = "confirmed"

	// StatusApplied means the tier is on the user record.
	StatusApplied Status = "applied"

	// StatusRejected is an admin refusing an attempt, with the reason in note.
	StatusRejected Status = "rejected"
)
