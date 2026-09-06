package players_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/dedo1911/ingress-plus-backend/internal/players"
)

const samplePlayerID = "cb3773292130450080439d75cdcff215.c"

func TestIsPlayerID(t *testing.T) {
	scenarios := []struct {
		raw  string
		want bool
	}{
		{samplePlayerID, true},
		{"bdad80740e154097ab7aa238da864c03.c", true},
		{"CB3773292130450080439D75CDCFF215.c", true}, // same UUID, normalized

		// the values moderators leave behind when scrubbing a record, which
		// must never be hashed - see ErrNotAPlayerID
		{"", false},
		{"USER DELETED", false},
		{"UNKNOWN", false},

		// near-misses: a player ID is 32 hex characters plus ".c", and anything
		// that does not follow that exactly is invalid
		{"cb3773292130450080439d75cdcff215", false},    // no suffix
		{"cb3773292130450080439d75cdcff21.c", false},   // 31 hex chars
		{"cb3773292130450080439d75cdcff2155.c", false}, // 33 hex chars
		{"cb3773292130450080439d75cdcff215.", false},   // empty suffix
		{"cb3773292130450080439d75cdcff215.d", false},  // wrong suffix
		{"cb3773292130450080439d75cdcff215.cc", false}, // suffix too long
		{"zb3773292130450080439d75cdcff215.c", false},  // not hex
		{" cb3773292130450080439d75cdcff215.c", false}, // stray whitespace
	}

	for _, s := range scenarios {
		if got := players.IsPlayerID(s.raw); got != s.want {
			t.Errorf("IsPlayerID(%q) = %v, want %v", s.raw, got, s.want)
		}
	}
}

func TestHashIsStable(t *testing.T) {
	hasher, err := players.NewHasher("test-pepper")
	if err != nil {
		t.Fatal(err)
	}

	first, err := hasher.Hash(samplePlayerID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := hasher.Hash(samplePlayerID)
	if err != nil {
		t.Fatal(err)
	}

	// The whole feature rests on this: the same player hashing identically on
	// every upload is what links their contributions together.
	if first != second {
		t.Fatalf("same input produced different hashes: %q vs %q", first, second)
	}
	if len(first) != 64 {
		t.Fatalf("expected a 64-character hex digest, got %d characters", len(first))
	}
	if first == samplePlayerID {
		t.Fatal("hash returned the raw player ID")
	}
}

// An agent must not be split into two identities by a difference in casing, so
// a player ID that differs only in case has to hash to the same value.
func TestHashIsCaseInsensitive(t *testing.T) {
	hasher, err := players.NewHasher("test-pepper")
	if err != nil {
		t.Fatal(err)
	}

	lower, err := hasher.Hash(samplePlayerID)
	if err != nil {
		t.Fatal(err)
	}
	upper, err := hasher.Hash(strings.ToUpper(samplePlayerID[:32]) + ".c")
	if err != nil {
		t.Fatal(err)
	}

	if lower != upper {
		t.Fatalf("the same player ID in different cases produced two identities: %q vs %q", lower, upper)
	}
}

func TestHashDiffersPerPlayerAndPepper(t *testing.T) {
	hasher, err := players.NewHasher("test-pepper")
	if err != nil {
		t.Fatal(err)
	}
	other, err := players.NewHasher("different-pepper")
	if err != nil {
		t.Fatal(err)
	}

	mine, err := hasher.Hash(samplePlayerID)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := hasher.Hash("bdad80740e154097ab7aa238da864c03.c")
	if err != nil {
		t.Fatal(err)
	}
	if mine == theirs {
		t.Fatal("two different players hashed to the same value")
	}

	// A different pepper must produce a different digest, otherwise the pepper
	// is not actually keying anything and a scraper who kept the old public
	// player IDs could rebuild the mapping.
	rehashed, err := other.Hash(samplePlayerID)
	if err != nil {
		t.Fatal(err)
	}
	if rehashed == mine {
		t.Fatal("pepper had no effect on the digest")
	}
}

func TestHashRejectsScrubbedValues(t *testing.T) {
	hasher, err := players.NewHasher("test-pepper")
	if err != nil {
		t.Fatal(err)
	}

	// Hashing these would collapse every scrubbed record onto one shared hash,
	// which would then read as a single extremely prolific agent. "" and
	// "USER DELETED" are the only two non-conforming values in the production
	// table today; the rest are guards against future malformed input.
	for _, raw := range []string{"", "USER DELETED", "UNKNOWN", "cb3773292130450080439d75cdcff215.d", "not-an-id"} {
		if _, err := hasher.Hash(raw); !errors.Is(err, players.ErrNotAPlayerID) {
			t.Errorf("Hash(%q) error = %v, want ErrNotAPlayerID", raw, err)
		}
	}
}

func TestNewHasherRejectsEmptyPepper(t *testing.T) {
	if _, err := players.NewHasher(""); err == nil {
		t.Fatal("expected an error for an empty pepper, got nil")
	}
}
