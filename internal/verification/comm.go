package verification

import (
	"fmt"
	"regexp"
)

const (
	// Point Nemo, the oceanic pole of inaccessibility. No portals, no players,
	// and COMM there is empty, so an admin scrolling the feed sees only
	// verification posts - and a stray probe bothers nobody.
	//
	// Intel accepts a plext at whatever coordinates the caller supplies,
	// verified in a live test on 2026-09-06, which is the whole reason this
	// flow can exist without spamming a real area's COMM.
	commLatE6 = -48876667
	commLngE6 = -123393333

	// commPrefix is what the admin plugin scans COMM for. The code follows it
	// directly; keep CommPattern in step with this.
	commPrefix = "Ingress Plus verification "
)

// CommPattern extracts a code from a COMM plext. Exported because the admin
// tooling matches the same shape - if this ever has to change, both halves are
// in one place.
var CommPattern = regexp.MustCompile(commPrefix + regexp.QuoteMeta(codePrefix) + `[` + codeAlphabet + `]{` + fmt.Sprint(codeLength) + `}`)

// Comm is what the plugin should post, and where.
//
// The backend supplies it rather than the plugin hardcoding it: if the location
// ever has to move - Niantic blocking it, or the spot getting noisy - that is a
// backend change, not a plugin release chasing auto-update across a userbase
// that opens the plugin about once a month.
type Comm struct {
	Message string
	LatE6   int
	LngE6   int
}

// CommTarget returns the plext for a verification code.
func CommTarget(code string) Comm {
	return Comm{
		Message: commPrefix + code,
		LatE6:   commLatE6,
		LngE6:   commLngE6,
	}
}
