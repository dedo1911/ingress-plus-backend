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
	"strings"
)

// PepperEnvVar names the environment variable holding the HMAC key.
const PepperEnvVar = "PLAYER_ID_HASH_PEPPER"

// ErrNotAPlayerID is returned for anything that is not a well-formed Ingress
// player ID. Hashing such a value would be actively harmful: every record
// sharing it would collapse onto one hash and read as a single prolific agent.
var ErrNotAPlayerID = errors.New("not an Ingress player ID")

// playerIDPattern matches the only shape Ingress sends: a 32-character hex UUID
// followed by ".c", e.g. "cb3773292130450080439d75cdcff215.c". Anything else is
// invalid by definition.
//
// A format check rather than a denylist of known scrub markers on purpose - it
// also rejects whatever wording a future manual scrub happens to use. Verified
// against the full production table: of 1734 stored values, 1732 match this
// exactly (all lowercase, all ".c", no exceptions) and the only two that do not
// are "" and "USER DELETED". A further 20 records carry no playerId key at all.
var playerIDPattern = regexp.MustCompile(`^[0-9a-f]{32}\.c$`)

// normalize lowercases raw so that the same ID written in different cases can
// never hash to two values and split one agent into two identities. Everything
// in production is already lowercase; this just makes that guarantee explicit
// rather than incidental.
func normalize(raw string) string {
	return strings.ToLower(raw)
}

// IsPlayerID reports whether raw is a well-formed Ingress player ID rather than
// a blank, a moderator's scrub marker, or anything else malformed.
func IsPlayerID(raw string) bool {
	return playerIDPattern.MatchString(normalize(raw))
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
	normalized := normalize(raw)
	if !playerIDPattern.MatchString(normalized) {
		return "", fmt.Errorf("%q: %w", raw, ErrNotAPlayerID)
	}

	mac := hmac.New(sha256.New, h.pepper)
	mac.Write([]byte(normalized))
	return hex.EncodeToString(mac.Sum(nil)), nil
}
