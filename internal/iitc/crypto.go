package iitc

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// HKDFInfo binds the derived key to this protocol. The sideloader must use
// the same string; bump the version suffix if the scheme ever changes.
const HKDFInfo = "ingress-plus-iitc-v1"

// Sealed is one encrypted plugin payload, ready to be base64-encoded into the
// /load response.
type Sealed struct {
	ServerPub  []byte
	IV         []byte
	Ciphertext []byte
}

// ErrBadClientKey means the client sent something that isn't an uncompressed
// P-256 point, as WebCrypto's exportKey("raw") produces.
var ErrBadClientKey = errors.New("client public key must be a raw uncompressed P-256 point")

// deriveKey runs HKDF-SHA256 over the ECDH shared secret. The salt is empty,
// which HKDF treats as a block of zeroes; WebCrypto accepts an empty salt too.
func deriveKey(secret []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, secret, nil, HKDFInfo, 32)
}

// Seal encrypts plaintext for the holder of clientPubRaw.
//
// Each call uses a fresh server key pair, so every response is encrypted
// under a different key and a captured response is useless without the
// client's private key, which never leaves the browser tab that made it.
//
// This keeps the code out of the browser's network log, proxies and naive
// scrapers. It is not DRM: the browser decrypts the code to run it, so the
// person running it can always read it. That limit is the reason every copy
// is also watermarked.
func Seal(clientPubRaw, plaintext []byte) (*Sealed, error) {
	curve := ecdh.P256()

	clientPub, err := curve.NewPublicKey(clientPubRaw)
	if err != nil {
		return nil, ErrBadClientKey
	}

	serverPriv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating server key: %w", err)
	}

	secret, err := serverPriv.ECDH(clientPub)
	if err != nil {
		return nil, ErrBadClientKey
	}

	key, err := deriveKey(secret)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}

	return &Sealed{
		ServerPub:  serverPriv.PublicKey().Bytes(),
		IV:         iv,
		Ciphertext: gcm.Seal(nil, iv, plaintext, nil),
	}, nil
}

// Open is the client side of Seal, used by tests to check the round trip
// with the same primitives the sideloader uses through WebCrypto.
func Open(clientPriv *ecdh.PrivateKey, sealed *Sealed) ([]byte, error) {
	serverPub, err := ecdh.P256().NewPublicKey(sealed.ServerPub)
	if err != nil {
		return nil, err
	}
	secret, err := clientPriv.ECDH(serverPub)
	if err != nil {
		return nil, err
	}
	key, err := deriveKey(secret)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, sealed.IV, sealed.Ciphertext, nil)
}
