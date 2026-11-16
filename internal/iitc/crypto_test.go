package iitc

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

func TestSealOpenRoundTrip(t *testing.T) {
	clientPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("// ==UserScript==\nwindow.plugin = function () {}\n")

	sealed, err := Seal(clientPriv.PublicKey().Bytes(), plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed.Ciphertext, []byte("UserScript")) {
		t.Fatal("ciphertext contains the plaintext")
	}
	if len(sealed.ServerPub) != 65 || len(sealed.IV) != 12 {
		t.Fatalf("unexpected sizes: serverPub=%d iv=%d", len(sealed.ServerPub), len(sealed.IV))
	}

	got, err := Open(clientPriv, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round trip mismatch: %q", got)
	}

	// Another client's key can't open it.
	other, _ := ecdh.P256().GenerateKey(rand.Reader)
	if _, err := Open(other, sealed); err == nil {
		t.Fatal("a different key opened the payload")
	}

	// Two seals of the same plaintext differ (fresh server key per call).
	again, _ := Seal(clientPriv.PublicKey().Bytes(), plaintext)
	if bytes.Equal(again.ServerPub, sealed.ServerPub) || bytes.Equal(again.Ciphertext, sealed.Ciphertext) {
		t.Fatal("server key or ciphertext reused between calls")
	}
}

func TestSealRejectsBadKey(t *testing.T) {
	for _, key := range [][]byte{nil, []byte("short"), make([]byte, 65)} {
		if _, err := Seal(key, []byte("x")); err != ErrBadClientKey {
			t.Errorf("Seal(%x) err = %v, want ErrBadClientKey", key, err)
		}
	}
}

func TestWatermark(t *testing.T) {
	if _, err := NewWatermarker(""); err == nil {
		t.Fatal("empty key accepted")
	}
	w, _ := NewWatermarker("k")
	other, _ := NewWatermarker("other")

	if w.Fingerprint("u1") == w.Fingerprint("u2") {
		t.Fatal("different users share a fingerprint")
	}
	if w.Fingerprint("u1") == other.Fingerprint("u1") {
		t.Fatal("fingerprint doesn't depend on the key")
	}

	code := []byte("'use strict'\nfoo() // trailing comment")
	out := string(w.Apply(code, "slug", "1.0.0", "u1", time.Unix(0, 0)))
	fp := w.Fingerprint("u1")

	if !strings.HasPrefix(out, "// Served by Ingress Plus: slug@1.0.0, licensed to agent "+fp) {
		t.Fatalf("missing header: %q", out)
	}
	// The original code must follow the header comment line untouched, so a
	// leading "use strict" directive stays a directive.
	if !strings.Contains(out, "\n'use strict'\nfoo() // trailing comment\n;void \"ipx:"+fp+"\";\n") {
		t.Fatalf("code or trailer mangled: %q", out)
	}
}
