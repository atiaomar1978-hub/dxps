package breaker

import (
	"testing"
	"time"
)

// TC-BRK-001: The breaker opens after N consecutive failures and rejects calls during cool-down.
func TestOpens(t *testing.T) {
	now := time.Unix(0, 0)
	b := New(3, 10*time.Second)
	b.SetClock(func() time.Time { return now })
	for i := 0; i < 2; i++ {
		b.Record(false)
	}
	b.Record(true)
	if b.State() != Closed {
		t.Fatal("success must reset")
	}
	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatal("closed must allow")
		}
		b.Record(false)
	}
	if b.State() != Open || b.Allow() || b.State().String() != "open" {
		t.Fatal("should be open")
	}
}

// TC-BRK-002: After cool-down exactly one probe is admitted; its result closes or re-opens the breaker.
func TestHalfOpen(t *testing.T) {
	now := time.Unix(0, 0)
	b := New(1, time.Second)
	b.SetClock(func() time.Time { return now })
	b.Record(false)
	now = now.Add(2 * time.Second)
	if !b.Allow() || b.State() != HalfOpen || b.Allow() {
		t.Fatal("single probe expected")
	}
	b.Record(false)
	if b.State() != Open {
		t.Fatal("failed probe must reopen")
	}
	now = now.Add(2 * time.Second)
	b.Allow()
	b.Record(true)
	if b.State() != Closed || !b.Allow() || HalfOpen.String() != "half-open" {
		t.Fatal("successful probe must close")
	}
	// half-open with probing released (Record(true) not yet called) allows the next probe
	b2 := New(0, 0)
	b2.Record(false)
	b2.Allow()
	b2.mu.Lock()
	b2.probing = false
	b2.mu.Unlock()
	if !b2.Allow() {
		t.Fatal("released probe slot should allow")
	}
}
