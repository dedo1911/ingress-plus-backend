package iitc

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"
)

// WatermarkKeyEnvVar names the HMAC key used to fingerprint served copies.
const WatermarkKeyEnvVar = "IITC_WATERMARK_KEY"

// Watermarker stamps every copy of a plugin with a fingerprint of the agent
// it was served to. The fingerprint is an HMAC of the user id, so a leaked
// copy can be traced back by an admin (who can recompute it for any user)
// without the copy itself revealing whose it is.
type Watermarker struct {
	key []byte
}

// NewWatermarker builds a Watermarker from an explicit key. Used by tests.
func NewWatermarker(key string) (*Watermarker, error) {
	if key == "" {
		return nil, errors.New("IITC watermark key is empty")
	}
	return &Watermarker{key: []byte(key)}, nil
}

// NewWatermarkerFromEnv reads the key from the environment. Without it the
// fingerprints would be a plain digest of a user id anyone can see on a
// public profile, so callers treat a failure here as fatal at startup.
func NewWatermarkerFromEnv() (*Watermarker, error) {
	key := os.Getenv(WatermarkKeyEnvVar)
	if key == "" {
		return nil, fmt.Errorf("%s is not set - refusing to serve IITC plugins without a watermark key", WatermarkKeyEnvVar)
	}
	return NewWatermarker(key)
}

// Fingerprint returns the short per-user marker embedded in served copies.
func (w *Watermarker) Fingerprint(userID string) string {
	mac := hmac.New(sha256.New, w.key)
	mac.Write([]byte(userID))
	return hex.EncodeToString(mac.Sum(nil))[:24]
}

// Apply returns code with the fingerprint added twice: a header comment that
// says plainly who the copy belongs to, and a no-op expression at the very
// end, which survives someone deleting the header.
//
// Nothing is inserted before the code itself: a statement in front of a
// "use strict" directive would silently turn strict mode off. The trailing
// newline and semicolon keep the marker from joining a final line comment
// or an expression that has no semicolon.
func (w *Watermarker) Apply(code []byte, slug, version, userID string, now time.Time) []byte {
	fp := w.Fingerprint(userID)
	header := fmt.Sprintf("// Served by Ingress Plus: %s@%s, licensed to agent %s on %s. Do not redistribute.\n",
		slug, version, fp, now.UTC().Format(time.RFC3339))
	trailer := fmt.Sprintf("\n;void \"ipx:%s\";\n", fp)

	out := make([]byte, 0, len(header)+len(code)+len(trailer))
	out = append(out, header...)
	out = append(out, code...)
	out = append(out, trailer...)
	return out
}
