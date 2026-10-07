package secretbox

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func box(t *testing.T) *Box {
	b, err := New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TC-SBX-001: Webhook secrets round-trip through AES-256-GCM and are never stored in clear.
func TestRoundTrip(t *testing.T) {
	b := box(t)
	ct, err := b.Seal("mvno-alpha", []byte("s3cr3t-webhook-key"))
	if err != nil || !strings.HasPrefix(ct, "enc:v1:") || strings.Contains(ct, "s3cr3t") {
		t.Fatalf("seal: %v %q", err, ct)
	}
	pt, err := b.Open("mvno-alpha", ct)
	if err != nil || string(pt) != "s3cr3t-webhook-key" {
		t.Fatalf("open: %v", err)
	}
	ct2, _ := b.Seal("mvno-alpha", []byte("s3cr3t-webhook-key"))
	if ct == ct2 {
		t.Fatal("nonce reuse")
	}
}

// TC-SBX-002: A ciphertext is bound to its tenant (AAD) - it cannot be opened under another tenant.
func TestTenantBinding(t *testing.T) {
	b := box(t)
	ct, _ := b.Seal("mvno-alpha", []byte("x"))
	if _, err := b.Open("mvno-beta", ct); !errors.Is(err, ErrCiphertext) {
		t.Fatalf("cross-tenant open: %v", err)
	}
}

// TC-SBX-003: Tampered, truncated or foreign-key ciphertexts and bad keys are rejected.
func TestTamper(t *testing.T) {
	b := box(t)
	ct, _ := b.Seal("t", []byte("hello"))
	raw := []byte(ct)
	raw[len(raw)-2] ^= 1
	other, _ := New(bytes.Repeat([]byte{9}, 32))
	for _, bad := range []string{string(raw), "plain", "enc:v1:!!", "enc:v1:AAAA"} {
		if _, err := b.Open("t", bad); !errors.Is(err, ErrCiphertext) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if _, err := other.Open("t", ct); !errors.Is(err, ErrCiphertext) {
		t.Fatal("foreign key opened")
	}
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
}
