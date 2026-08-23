// Package players turns the raw Ingress player IDs that arrive with a media
// upload into stable, non-reversible identifiers, and keeps one "players"
// record per Ingress account so uploads can be grouped (and later linked to an
// Ingress Plus account) without storing the original ID anywhere.
package players

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
)

// PepperEnvVar names the environment variable holding the HMAC key.
const PepperEnvVar = "PLAYER_ID_HASH_PEPPER"

// ErrNotAPlayerID is returned for values that are not real Ingress player IDs:
// the empty string, and the sentinels moderators write when scrubbing a record
// on request ("USER DELETED"). Hashing those would be actively harmful - every
// scrubbed record would collapse onto one shared hash and read as a single
// prolific agent.
var ErrNotAPlayerID = errors.New("not an Ingress player ID")

// playerIDPattern matches the only shape Ingress has ever sent: 32 lowercase
// hex characters, a dot, and a short suffix (in practice always "c"), e.g.
// "cb3773292130450080439d75cdcff215.c". Every one of the 1732 non-scrubbed
// production records matches it.
//
// This is a format check rather than a denylist of known sentinels on purpose:
// it also rejects whatever wording a future manual scrub happens to use.
var playerIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}\.[0-9A-Za-z]+$`)

// IsPlayerID reports whether raw looks like a real Ingress player ID rather
// than a blank or a moderator's scrub marker.
func IsPlayerID(raw string) bool {
	return playerIDPattern.MatchString(raw)
}

// Hasher derives the stored identifier for a player ID.
//
// It is an HMAC rather than a bare SHA-256 because the raw IDs are not secret:
// "medias" has listRule "approved = true" and every record is auto-approved, so
// until this change ships every player ID was readable straight from the public
// API. Anyone who scraped it holds exactly the input set needed to rebuild a
// rainbow table, and an unkeyed digest would be reversible for precisely the
// players we are trying to protect. The pepper is what they don't have.
//
// The flip side: the pepper can never be rotated or lost once the raw IDs have
// been stripped, because there is nothing left to re-derive the hashes from.
type Hasher struct {
	pepper []byte
}

// NewHasher builds a Hasher from an explicit pepper. Used by tests; production
// goes through NewHasherFromEnv.
func NewHasher(pepper string) (*Hasher, error) {
	if pepper == "" {
		return nil, errors.New("player ID hash pepper is empty")
	}
	return &Hasher{pepper: []byte(pepper)}, nil
}

// NewHasherFromEnv reads the pepper from the environment. Callers are expected
// to treat a failure here as fatal at startup: continuing without a pepper
// would silently write reversible hashes.
func NewHasherFromEnv() (*Hasher, error) {
	pepper := os.Getenv(PepperEnvVar)
	if pepper == "" {
		return nil, fmt.Errorf("%s is not set - refusing to hash player IDs without a pepper", PepperEnvVar)
	}
	return NewHasher(pepper)
}

// Hash returns the stored identifier for raw, or ErrNotAPlayerID if raw is not
// a real player ID. The same input always yields the same output, which is what
// lets separate uploads from one agent be grouped together.
func (h *Hasher) Hash(raw string) (string, error) {
	if !IsPlayerID(raw) {
		return "", fmt.Errorf("%q: %w", raw, ErrNotAPlayerID)
	}

	mac := hmac.New(sha256.New, h.pepper)
	mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil)), nil
}
