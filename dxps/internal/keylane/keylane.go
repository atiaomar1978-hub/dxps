// Package keylane implements the key-lane executor (LLD 6.5): records with the same key run serially
// in arrival order, different keys run in parallel (bounded), and only the highest contiguous
// completed offset per partition becomes committable.
package keylane

import (
	"context"
	"sync"
)

type TP struct {
	Topic     string
	Partition int32
}

type Record struct {
	TP
	Offset int64
	Key    string
	Value  any
}

type Handler func(ctx context.Context, r Record) error

// Tracker tracks in-flight offsets of one partition (offsets begin in increasing order).
type Tracker struct {
	inflight []int64
	done     map[int64]bool
}

func NewTracker() *Tracker { return &Tracker{done: map[int64]bool{}} }

func (t *Tracker) Begin(off int64) { t.inflight = append(t.inflight, off) }

// Complete marks off done and returns the next offset to commit when the contiguous prefix advanced.
func (t *Tracker) Complete(off int64) (int64, bool) {
	t.done[off] = true
	var next int64 = -1
	for len(t.inflight) > 0 && t.done[t.inflight[0]] {
		delete(t.done, t.inflight[0])
		next = t.inflight[0] + 1
		t.inflight = t.inflight[1:]
	}
	return next, next >= 0
}

func (t *Tracker) InFlight() int { return len(t.inflight) }

type lane struct {
	q       []Record
	running bool
}

type Executor struct {
	ctx      context.Context
	h        Handler
	onErr    func(Record, error)
	sem      chan struct{}
	mu       sync.Mutex
	lanes    map[string]*lane
	trackers map[TP]*Tracker
	commits  map[TP]int64
	wg       sync.WaitGroup
	maxLane  int
}

// New creates an executor with a global concurrency cap. onErr is called for failed records; the
// record still completes (a poison message never blocks its lane - the caller routes it to retry/DLQ).
func New(ctx context.Context, concurrency int, h Handler, onErr func(Record, error)) *Executor {
	if onErr == nil {
		onErr = func(Record, error) {}
	}
	return &Executor{ctx: ctx, h: h, onErr: onErr, sem: make(chan struct{}, max(concurrency, 1)),
		lanes: map[string]*lane{}, trackers: map[TP]*Tracker{}, commits: map[TP]int64{}}
}

func (e *Executor) Submit(r Record) {
	e.mu.Lock()
	tr := e.trackers[r.TP]
	if tr == nil {
		tr = NewTracker()
		e.trackers[r.TP] = tr
	}
	tr.Begin(r.Offset)
	l := e.lanes[r.Key]
	if l == nil {
		l = &lane{}
		e.lanes[r.Key] = l
	}
	l.q = append(l.q, r)
	e.maxLane = max(e.maxLane, len(l.q))
	start := !l.running
	l.running = true
	e.mu.Unlock()
	if start {
		e.wg.Add(1)
		go e.run(r.Key, l)
	}
}

func (e *Executor) run(key string, l *lane) {
	defer e.wg.Done()
	for {
		e.mu.Lock()
		if len(l.q) == 0 {
			l.running = false
			delete(e.lanes, key) // evict idle lane
			e.mu.Unlock()
			return
		}
		r := l.q[0]
		l.q = l.q[1:]
		e.mu.Unlock()

		e.sem <- struct{}{}
		err := e.h(e.ctx, r)
		<-e.sem
		if err != nil {
			e.onErr(r, err)
		}
		e.complete(r)
	}
}

func (e *Executor) complete(r Record) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tr := e.trackers[r.TP]
	if tr == nil { // partition revoked meanwhile
		return
	}
	if next, ok := tr.Complete(r.Offset); ok {
		e.commits[r.TP] = next
	}
}

// TakeCommits returns and clears the committable offsets (next offset to read per partition).
func (e *Executor) TakeCommits() map[TP]int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.commits) == 0 {
		return nil
	}
	out := e.commits
	e.commits = map[TP]int64{}
	return out
}

// Revoke forgets partitions (after a rebalance); in-flight records still finish but are not committed.
func (e *Executor) Revoke(tps ...TP) {
	e.mu.Lock()
	for _, tp := range tps {
		delete(e.trackers, tp)
		delete(e.commits, tp)
	}
	e.mu.Unlock()
}

// Wait blocks until all lanes are idle.
func (e *Executor) Wait() { e.wg.Wait() }

type Stats struct {
	Lanes, InFlight, MaxLaneDepth int
}

func (e *Executor) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Stats{Lanes: len(e.lanes), MaxLaneDepth: e.maxLane}
	for _, t := range e.trackers {
		s.InFlight += t.InFlight()
	}
	return s
}
