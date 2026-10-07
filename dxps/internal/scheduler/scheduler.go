// Package scheduler implements the DxPS weighted-fair priority scheduler (LLD 6.4): strict P0,
// weighted round robin 4:2:1 for P1..P3, and deficit round robin across tenants inside each tier.
// Capacity is bounded; Push blocks when full (backpressure to the Kafka poller).
package scheduler

import (
	"context"
	"sync"

	"dxps/internal/contract"
)

var Weights = [4]int{8, 4, 2, 1}

type Item[T any] struct {
	Tenant string
	Tier   contract.Priority
	V      T
}

type tenantQ[T any] struct {
	id      string
	q       []Item[T]
	deficit int
}

type tierQ[T any] struct {
	active []*tenantQ[T]
	byID   map[string]*tenantQ[T]
	cursor int
	n      int
}

func (tq *tierQ[T]) push(it Item[T]) {
	t := tq.byID[it.Tenant]
	if t == nil {
		t = &tenantQ[T]{id: it.Tenant}
		tq.byID[it.Tenant] = t
	}
	if len(t.q) == 0 {
		tq.active = append(tq.active, t)
	}
	t.q = append(t.q, it)
	tq.n++
}

// next serves the tenant at the cursor up to its quantum, then moves on (DRR, unit cost per item).
func (tq *tierQ[T]) next(quantum func(string) int) (Item[T], bool) {
	var zero Item[T]
	if tq.n == 0 {
		return zero, false
	}
	if tq.cursor >= len(tq.active) {
		tq.cursor = 0
	}
	t := tq.active[tq.cursor]
	if t.deficit <= 0 {
		t.deficit = max(1, quantum(t.id))
	}
	it := t.q[0]
	t.q[0] = zero
	t.q = t.q[1:]
	t.deficit--
	tq.n--
	if len(t.q) == 0 { // idle tenant loses its remaining credit and leaves the ring
		t.deficit = 0
		tq.active = append(tq.active[:tq.cursor], tq.active[tq.cursor+1:]...)
		delete(tq.byID, t.id)
	} else if t.deficit == 0 {
		tq.cursor++
	}
	return it, true
}

type Stats struct {
	Queued   [4]int            `json:"queued"`
	Served   [4]uint64         `json:"served"`
	ByTenant map[string]uint64 `json:"byTenant"`
}

type Scheduler[T any] struct {
	mu      sync.Mutex
	tiers   [4]*tierQ[T]
	cap     int
	size    int
	cursor  int // current WRR tier (1..3)
	credit  int
	quantum func(string) int
	wake    chan struct{}
	space   chan struct{}
	served  [4]uint64
	byTen   map[string]uint64
}

func New[T any](capacity int, quantum func(tenant string) int) *Scheduler[T] {
	if quantum == nil {
		quantum = func(string) int { return 1 }
	}
	s := &Scheduler[T]{cap: max(capacity, 1), quantum: quantum, cursor: 1, credit: Weights[1],
		wake: make(chan struct{}, 1), space: make(chan struct{}, 1), byTen: map[string]uint64{}}
	for i := range s.tiers {
		s.tiers[i] = &tierQ[T]{byID: map[string]*tenantQ[T]{}}
	}
	return s
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// Push enqueues an item, blocking while the scheduler is full.
func (s *Scheduler[T]) Push(ctx context.Context, it Item[T]) error {
	if !it.Tier.Valid() {
		it.Tier = contract.P3
	}
	for {
		s.mu.Lock()
		if s.size < s.cap {
			s.tiers[it.Tier].push(it)
			s.size++
			s.mu.Unlock()
			signal(s.wake)
			return nil
		}
		s.mu.Unlock()
		select {
		case <-s.space:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// TryNext returns the next item without blocking.
func (s *Scheduler[T]) TryNext() (Item[T], bool) {
	s.mu.Lock()
	it, ok := s.pick()
	more, room := s.size > 0, s.size < s.cap
	s.mu.Unlock()
	if ok {
		signal(s.space)
		if more {
			signal(s.wake)
		}
	} else if room {
		signal(s.space)
	}
	return it, ok
}

// Next blocks until an item is available or ctx is done.
func (s *Scheduler[T]) Next(ctx context.Context) (Item[T], error) {
	for {
		if it, ok := s.TryNext(); ok {
			return it, nil
		}
		select {
		case <-s.wake:
		case <-ctx.Done():
			var zero Item[T]
			return zero, ctx.Err()
		}
	}
}

func (s *Scheduler[T]) pick() (Item[T], bool) {
	if it, ok := s.tiers[contract.P0].next(s.quantum); ok { // strict pre-emption for P0
		return s.served1(it), true
	}
	for tries := 0; tries < 6; tries++ {
		tq := s.tiers[s.cursor]
		if tq.n > 0 && s.credit > 0 {
			it, _ := tq.next(s.quantum)
			s.credit--
			return s.served1(it), true
		}
		s.cursor++
		if s.cursor > 3 {
			s.cursor = 1
		}
		s.credit = Weights[s.cursor]
	}
	var zero Item[T]
	return zero, false
}

func (s *Scheduler[T]) served1(it Item[T]) Item[T] {
	s.size--
	s.served[it.Tier]++
	s.byTen[it.Tenant]++
	return it
}

func (s *Scheduler[T]) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

func (s *Scheduler[T]) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{Served: s.served, ByTenant: make(map[string]uint64, len(s.byTen))}
	for i, t := range s.tiers {
		st.Queued[i] = t.n
	}
	for k, v := range s.byTen {
		st.ByTenant[k] = v
	}
	return st
}
