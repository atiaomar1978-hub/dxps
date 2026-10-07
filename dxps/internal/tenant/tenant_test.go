package tenant

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// TC-TEN-001: Tenant identifiers follow the dxps_tenant domain (1..40 safe characters).
func TestValid(t *testing.T) {
	for _, ok := range []string{"host-mno", "mvno_alpha.1", "GLOBAL", strings.Repeat("a", 40)} {
		if !Valid(ok) {
			t.Fatalf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", strings.Repeat("a", 41), "a b", "a'b", "a;b", "ä", "a/b"} {
		if Valid(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// TC-TEN-002: Require enforces valid, known and ACTIVE tenants (DXPS-1004).
func TestRequire(t *testing.T) {
	r := NewRegistry(Tenant{ID: "a", Status: Active, SLAWeight: 4}, Tenant{ID: "s", Status: Suspended})
	if _, err := r.Require("a"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]error{"bad id": ErrInvalidID, "zz": ErrUnknown, "s": ErrInactive} {
		if _, err := r.Require(id); !errors.Is(err, want) {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

// TC-TEN-003: Registry replace/all/quantum are consistent and concurrency safe.
func TestRegistry(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); r.Replace([]Tenant{{ID: "b", SLAWeight: 2}, {ID: "a"}}) }()
		go func() { defer wg.Done(); _ = r.All(); _ = r.Quantum("b") }()
	}
	wg.Wait()
	all := r.All()
	if len(all) != 2 || all[0].ID != "a" || r.Quantum("b") != 2 || r.Quantum("a") != 1 || r.Quantum("none") != 1 {
		t.Fatalf("unexpected %v", all)
	}
	if _, ok := r.Get("a"); !ok {
		t.Fatal("missing a")
	}
}

// TC-TEN-004: Tenant context propagation.
func TestContext(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("empty context has tenant")
	}
	if v, ok := FromContext(WithTenant(context.Background(), "x")); !ok || v != "x" {
		t.Fatal("tenant lost")
	}
	if _, ok := FromContext(WithTenant(context.Background(), "")); ok {
		t.Fatal("empty tenant accepted")
	}
}
