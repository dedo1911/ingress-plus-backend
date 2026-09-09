package verification

import (
	"fmt"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

// MismatchError says an agent's Ingress Plus profile disagrees with what
// Ingress reports. It carries both sides because the message is shown to the
// agent - the plugin puts response bodies straight into an alert box - and
// "they don't match" without saying what to change is useless.
type MismatchError struct {
	Field    string
	Reported string
	OnSite   string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf(
		"Your Ingress %s (%s) doesn't match your Ingress Plus %s (%s). "+
			"Fix it at https://ingress.plus/verify and try again - your code is still valid.",
		e.Field, e.Reported, e.Field, orNone(e.OnSite),
	)
}

func orNone(value string) string {
	if value == "" {
		return "not set"
	}
	return value
}

// CheckIdentity is the gate every tier passes through: nothing is granted
// unless the agent's site profile already says who Ingress says they are. The
// backend never quietly rewrites a profile to make a verification succeed.
//
// It is a gate, NOT the proof. For advanced and strong the values passed in
// come from a page the claimant controls, and this runs only so a mismatched
// agent finds out at once instead of posting to COMM and waiting on an admin.
// The proof is the admin reading the plext back; Apply uses the values written
// by the confirm route, which are Niantic's.
//
// Comparison is case-insensitive on both fields: users.faction is stored
// lowercase while Ingress reports RESISTANCE. Note that a site faction of
// machina can never match a COMM plext, so those accounts stay manual admin
// grants - that is a consequence of the game, not a case to special-case here.
func CheckIdentity(user, verification *core.Record, nickname, faction string) error {
	if nickname == "" || faction == "" {
		return &MismatchError{Field: "agent name", Reported: orNone(nickname), OnSite: user.GetString("username")}
	}

	if !strings.EqualFold(faction, user.GetString("faction")) {
		return &MismatchError{Field: "faction", Reported: faction, OnSite: user.GetString("faction")}
	}

	// A contested attempt is the one case where the profile deliberately does
	// not hold the name: another account has it and usernames are unique, so
	// the agent could not have set it. The name they typed on /verify stands in
	// for the profile, and Apply is what actually takes it off the other
	// account - and only for a tier that proves something.
	expected := user.GetString("username")
	if verification.GetBool("contested") {
		expected = verification.GetString("claimed_username")
	}

	if !strings.EqualFold(nickname, expected) {
		return &MismatchError{Field: "agent name", Reported: nickname, OnSite: expected}
	}

	return nil
}
