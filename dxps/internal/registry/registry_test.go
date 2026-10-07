package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	"dxps/internal/breaker"
	"dxps/internal/contract"
)

func fixture() *Registry {
	return New(
		&NE{Tenant: "host-mno", ID: "u1", Code: "udr01", NFType: "UDR", MaxTPS: 1000, MaxConc: 2, ReservedPct: 20,
			Endpoints: []Endpoint{{BaseURI: "https://b", Weight: 1}, {BaseURI: "https://a", Weight: 10}},
			Access: map[string]Access{"mvno-alpha": {Tenant: "mvno-alpha", QuotaTPS: 1000, AllowedOps: []string{"udr.*"}}}},
		&NE{Tenant: "host-mno", ID: "o1", Code: "ocs01", NFType: "OCS", Access: map[string]Access{
			"mvno-beta": {Tenant: "mvno-beta", AllowedOps: []string{"bss.*"}}}},
		&NE{Tenant: "mvno-alpha", ID: "o2", Code: "ocs-alpha", NFType: "OCS"},
		&NE{Tenant: "host-mno", ID: "n1", Code: "nef01", NFType: "NEF", Access: map[string]Access{
			"mvno-alpha": {Tenant: "mvno-alpha", AllowedOps: []string{"camara.*"}}}},
	)
}

// TC-REG-001: A FULL MVNO uses its own NE first (own OCS) and falls back to entitled shared NEs.
func TestSelectOwnFirst(t *testing.T) {
	r := fixture()
	s, err := r.Select("mvno-alpha", "OCS", "bss.account.create")
	if err != nil || s.NE.Code != "ocs-alpha" {
		t.Fatalf("own OCS not chosen: %v %v", s.NE, err)
	}
	s, err = r.Select("mvno-beta", "OCS", "bss.account.create")
	if err != nil || s.NE.Code != "ocs01" {
		t.Fatalf("shared OCS not chosen: %v", err)
	}
	s, err = r.Select("mvno-alpha", "UDR", "udr.amData.put")
	if err != nil || s.NE.Code != "udr01" || s.Access.Tenant != "mvno-alpha" {
		t.Fatalf("shared UDR %v", err)
	}
}

// TC-REG-002: Tenants without an entitlement get DXPS-1005; unknown NF types DXPS-1003.
func TestEntitlement(t *testing.T) {
	r := fixture()
	if _, err := r.Select("mvno-beta", "NEF", "camara.qod.create"); !errors.Is(err, ErrNotEntitled) {
		t.Fatalf("got %v", err)
	}
	if _, err := r.Select("mvno-alpha", "UDR", "ims.impi.put"); !errors.Is(err, ErrNotEntitled) {
		t.Fatalf("op glob not enforced: %v", err)
	}
	if _, err := r.Select("host-mno", "HSS", "x"); !errors.Is(err, ErrNoNE) {
		t.Fatalf("got %v", err)
	}
	ne, _ := r.Get("host-mno", "u1")
	if _, err := ne.Acquire(context.Background(), "mvno-beta", "udr.amData.put", contract.P1); !errors.Is(err, ErrNotEntitled) {
		t.Fatalf("acquire without entitlement: %v", err)
	}
	if !(Access{AllowedOps: []string{"udr.*", "ims.impi.put"}}).Allows("ims.impi.put") || (Access{}).Allows("x") {
		t.Fatal("glob")
	}
}

// TC-REG-003: Endpoints are ordered by weight; NEs without endpoints are unresolvable.
func TestEndpoint(t *testing.T) {
	r := fixture()
	ne, ok := r.Get("host-mno", "u1")
	if !ok {
		t.Fatal("missing")
	}
	if ep, err := ne.Endpoint(); err != nil || ep.BaseURI != "https://a" {
		t.Fatalf("%v %v", ep, err)
	}
	o, _ := r.Get("host-mno", "o1")
	if _, err := o.Endpoint(); !errors.Is(err, ErrNoNE) {
		t.Fatal(err)
	}
	if len(r.All()) != 4 || r.All()[0].Code != "nef01" {
		t.Fatal("All not sorted by code")
	}
}

// TC-REG-004: The NE concurrency cap blocks extra callers until a slot is released.
func TestConcurrencyCap(t *testing.T) {
	r := fixture()
	ne, _ := r.Get("host-mno", "u1")
	ctx := context.Background()
	rel1, err1 := ne.Acquire(ctx, "host-mno", "udr.amData.put", contract.P2)
	rel2, err2 := ne.Acquire(ctx, "mvno-alpha", "udr.amData.put", contract.P0)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	c, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := ne.Acquire(c, "host-mno", "udr.amData.put", contract.P2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cap not enforced: %v", err)
	}
	rel1()
	rel3, err := ne.Acquire(ctx, "host-mno", "udr.amData.put", contract.P2)
	if err != nil {
		t.Fatal(err)
	}
	rel2()
	rel3()
}

// TC-REG-005: An open circuit breaker rejects calls to a failing NE.
func TestBreakerGate(t *testing.T) {
	ne := &NE{Tenant: "h", ID: "x", Breaker: breaker.New(1, time.Hour)}
	New(ne)
	ne.Breaker.Record(false)
	if _, err := ne.Acquire(context.Background(), "h", "op", contract.P1); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("got %v", err)
	}
}

// TC-REG-006: Per-tenant quota on a shared NE throttles a noisy MVNO without affecting others.
func TestTenantQuota(t *testing.T) {
	ne := &NE{Tenant: "host-mno", ID: "u", MaxTPS: 10000, MaxConc: 100,
		Access: map[string]Access{"mvno-beta": {Tenant: "mvno-beta", QuotaTPS: 10, AllowedOps: []string{"*"}}}}
	New(ne)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	n := 0
	for {
		rel, err := ne.Acquire(ctx, "mvno-beta", "op", contract.P2)
		if err != nil {
			break
		}
		rel()
		n++
	}
	if n > 4 {
		t.Fatalf("quota 10 tps allowed %d calls in 150 ms", n)
	}
	rel, err := ne.Acquire(context.Background(), "host-mno", "op", contract.P2)
	if err != nil {
		t.Fatal("owner throttled by MVNO quota")
	}
	rel()
}
