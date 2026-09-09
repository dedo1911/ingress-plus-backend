package verification

import "github.com/pocketbase/pocketbase/core"

// FlagName is the feature_flags record gating the whole flow. It already
// exists and is already read by the website's root layout; until now nothing
// on the backend honoured it, so switching it off hid the UI while leaving the
// routes open.
const FlagName = "VERIFICATION_ENABLED"

// Enabled reports whether agents may start a verification.
//
// Fails closed, matching the frontend, whose defaults are off: a flag that
// cannot be read is treated as disabled rather than as permission.
//
// Deliberately not checked by the confirm route - an admin has to be able to
// finish a verification that was already posted to COMM after the flag goes
// off, or flipping it strands whoever was mid-flow.
func Enabled(app core.App) bool {
	record, err := app.FindFirstRecordByData("feature_flags", "name", FlagName)
	if err != nil || record == nil {
		return false
	}
	return record.GetBool("enabled")
}
