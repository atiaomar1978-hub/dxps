// Package breaker is a small consecutive-failure circuit breaker (closed -> open -> half-open).
package breaker

import (
	"sync"
	"time"
)

type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string { return [...]string{"closed", "open", "half-open"}[s] }

type Breaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	failures  int
	state     State
	openedAt  time.Time
	probing   bool
	now       func() time.Time
}

func New(threshold int, cooldown time.Duration) *Breaker {
	if threshold < 1 {
		threshold = 1
	}
	return &Breaker{threshold: threshold, cooldown: cooldown, now: time.Now}
}

func (b *Breaker) SetClock(now func() time.Time) { b.now = now }

// Allow reports whether a call may proceed. In half-open state a single probe is allowed.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Open:
		if b.now().Sub(b.openedAt) < b.cooldown {
			return false
		}
		b.state, b.probing = HalfOpen, true
		return true
	case HalfOpen:
		if b.probing {
			return false
		}
		b.probing = true
		return true
	}
	return true
}

// Record reports a call outcome (ok = NE answered without a transport/5xx failure).
func (b *Breaker) Record(ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok {
		b.failures, b.state, b.probing = 0, Closed, false
		return
	}
	b.failures++
	if b.state == HalfOpen || b.failures >= b.threshold {
		b.state, b.openedAt, b.probing = Open, b.now(), false
	}
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
