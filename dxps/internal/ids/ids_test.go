package ids

import (
	"strings"
	"testing"
)

// TC-IDS-001: New identifiers are time-ordered UUIDv7 values.
func TestNewV7(t *testing.T) {
	a, b := New(), New()
	if a.Version() != 7 || b.String() <= a.String() || !ValidUUID(NewString()) {
		t.Fatalf("not ordered v7: %s %s", a, b)
	}
}

// TC-IDS-002: Derived ids are deterministic per tenant + idempotency key and cannot collide by concatenation.
func TestDerive(t *testing.T) {
	if Derive("order", "t1", "k") != Derive("order", "t1", "k") {
		t.Fatal("not deterministic")
	}
	if Derive("order", "t1", "k") == Derive("order", "t2", "k") {
		t.Fatal("tenant not part of id")
	}
	if Derive("ab", "c") == Derive("a", "bc") {
		t.Fatal("separator ambiguity")
	}
	if Derive("x").Version() != 5 {
		t.Fatal("not v5")
	}
}

// TC-IDS-003: Idempotency keys and UUID syntax validation.
func TestValidation(t *testing.T) {
	for k, want := range map[string]bool{"abcd1234": true, "a:b.c-d_e123": true, "short": false, strings.Repeat("a", 65): false, "has space1": false, "inj';--xx": false} {
		if ValidIdempotencyKey(k) != want {
			t.Fatalf("%q", k)
		}
	}
	u := NewString()
	if !ValidUUID(u) || ValidUUID("x") || ValidUUID(strings.Replace(u, "-", "g", 1)) || ValidUUID("{"+u[1:]) {
		t.Fatal("uuid validation")
	}
	if _, err := Parse(u); err != nil {
		t.Fatal(err)
	}
}
