package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"dxps/internal/contract"
)

func drain[T any](s *Scheduler[T], n int) []Item[T] {
	var out []Item[T]
	for i := 0; i < n; i++ {
		it, ok := s.TryNext()
		if !ok {
			break
		}
		out = append(out, it)
	}
	return out
}

// TC-SCH-001: P0 (fraud/lawful barring) is always served before any queued P1..P3 work.
func TestStrictP0(t *testing.T) {
	s := New[int](100, nil)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_ = s.Push(ctx, Item[int]{Tenant: "a", Tier: contract.P3, V: i})
		_ = s.Push(ctx, Item[int]{Tenant: "a", Tier: contract.P1, V: i})
	}
	_ = s.Push(ctx, Item[int]{Tenant: "b", Tier: contract.P0, V: 99})
	if it, _ := s.TryNext(); it.Tier != contract.P0 || it.V != 99 {
		t.Fatalf("P0 not first: %+v", it)
	}
}

// TC-SCH-002: P1:P2:P3 are served in weighted round robin 4:2:1 under saturation.
func TestWeightedRoundRobin(t *testing.T) {
	s := New[int](1000, nil)
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		for p := contract.P1; p <= contract.P3; p++ {
			_ = s.Push(ctx, Item[int]{Tenant: "a", Tier: p})
		}
	}
	var n [4]int
	for _, it := range drain(s, 70) {
		n[it.Tier]++
	}
	if n[1] != 40 || n[2] != 20 || n[3] != 10 {
		t.Fatalf("ratio %v, want 40/20/10", n)
	}
}

// TC-SCH-003: Inside a tier, tenants share by deficit round robin in proportion to their SLA weight;
// a noisy tenant cannot starve a small MVNO.
func TestTenantDRR(t *testing.T) {
	q := map[string]int{"host-mno": 8, "mvno-beta": 2}
	s := New[int](10000, func(t string) int { return q[t] })
	ctx := context.Background()
	for i := 0; i < 1000; i++ {
		_ = s.Push(ctx, Item[int]{Tenant: "host-mno", Tier: contract.P2})
	}
	for i := 0; i < 100; i++ {
		_ = s.Push(ctx, Item[int]{Tenant: "mvno-beta", Tier: contract.P2})
	}
	by := map[string]int{}
	for _, it := range drain(s, 100) {
		by[it.Tenant]++
	}
	if by["host-mno"] != 80 || by["mvno-beta"] != 20 {
		t.Fatalf("shares %v, want 80/20", by)
	}
	st := s.Stats()
	if st.ByTenant["mvno-beta"] != 20 || st.Served[2] != 100 || st.Queued[2] != 1000 {
		t.Fatalf("stats %+v", st)
	}
}

// TC-SCH-004: Bounded capacity applies backpressure; Push unblocks when space frees and honours ctx.
func TestBackpressure(t *testing.T) {
	s := New[int](2, nil)
	ctx := context.Background()
	_ = s.Push(ctx, Item[int]{Tenant: "a", Tier: contract.P1})
	_ = s.Push(ctx, Item[int]{Tenant: "a", Tier: 9}) // invalid tier is demoted to P3
	c, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := s.Push(c, Item[int]{Tenant: "a", Tier: contract.P1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("push on full: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Push(ctx, Item[int]{Tenant: "a", Tier: contract.P1}) }()
	time.Sleep(20 * time.Millisecond)
	if _, err := s.Next(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s.Len() != 2 || s.Stats().Queued[3] != 1 {
		t.Fatalf("len %d", s.Len())
	}
}

// TC-SCH-005: Next blocks until an item arrives and returns ctx errors when cancelled.
func TestNextBlocking(t *testing.T) {
	s := New[string](4, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); _ = s.Push(ctx, Item[string]{Tenant: "a", Tier: contract.P2, V: "x"}) }()
	if it, err := s.Next(ctx); err != nil || it.V != "x" {
		t.Fatalf("%v %v", it, err)
	}
	cancel()
	if _, err := s.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, ok := s.TryNext(); ok {
		t.Fatal("empty scheduler returned item")
	}
}
