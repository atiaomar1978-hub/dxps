// Package secretbox encrypts small secrets (webhook HMAC secrets) at rest with AES-256-GCM.
// The tenant is bound as additional authenticated data so a ciphertext cannot be replayed under another tenant.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
)

const prefix = "enc:v1:"

var ErrCiphertext = errors.New("invalid ciphertext")

type Box struct{ aead cipher.AEAD }

func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("master key must be 32 bytes")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &Box{aead: a}, nil
}

func (b *Box) Seal(tenantID string, plaintext []byte) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := b.aead.Seal(nonce, nonce, plaintext, []byte(tenantID))
	return prefix + base64.RawStdEncoding.EncodeToString(ct), nil
}

func (b *Box) Open(tenantID, s string) ([]byte, error) {
	if !strings.HasPrefix(s, prefix) {
		return nil, ErrCiphertext
	}
	raw, err := base64.RawStdEncoding.DecodeString(s[len(prefix):])
	if err != nil || len(raw) < b.aead.NonceSize()+b.aead.Overhead() {
		return nil, ErrCiphertext
	}
	n := b.aead.NonceSize()
	pt, err := b.aead.Open(nil, raw[:n], raw[n:], []byte(tenantID))
	if err != nil {
		return nil, ErrCiphertext
	}
	return pt, nil
}
