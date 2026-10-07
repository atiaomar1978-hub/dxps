package keylane

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TC-KLN-001: Records with the same key run strictly serially in arrival order.
func TestSameKeyOrdered(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]int64{}
	var concurrent, maxC atomic.Int32
	e := New(context.Background(), 32, func(_ context.Context, r Record) error {
		c := concurrent.Add(1)
		for {
			m := maxC.Load()
			if c <= m || maxC.CompareAndSwap(m, c) {
				break
			}
		}
		time.Sleep(time.Duration(rand.IntN(300)) * time.Microsecond)
		mu.Lock()
		seen[r.Key] = append(seen[r.Key], r.Offset)
		mu.Unlock()
		concurrent.Add(-1)
		return nil
	}, nil)
	tp := TP{"t", 0}
	for i := int64(0); i < 400; i++ {
		e.Submit(Record{TP: tp, Offset: i, Key: fmt.Sprintf("supi:%d", i%8)})
	}
	e.Wait()
	for k, offs := range seen {
		for i := 1; i < len(offs); i++ {
			if offs[i] < offs[i-1] {
				t.Fatalf("%s out of order: %v", k, offs)
			}
		}
	}
	if maxC.Load() < 2 {
		t.Fatal("different keys did not run in parallel")
	}
	if c := e.TakeCommits(); c[tp] != 400 {
		t.Fatalf("commit %v, want 400", c)
	}
}

// TC-KLN-002: Only the highest contiguous completed offset is committable (no data loss on crash).
func TestContiguousCommit(t *testing.T) {
	tr := NewTracker()
	for i := int64(10); i < 14; i++ {
		tr.Begin(i)
	}
	if _, ok := tr.Complete(12); ok {
		t.Fatal("gap committed")
	}
	if n, ok := tr.Complete(10); !ok || n != 11 {
		t.Fatalf("got %d", n)
	}
	if n, ok := tr.Complete(11); !ok || n != 13 || tr.InFlight() != 1 {
		t.Fatalf("got %d inflight %d", n, tr.InFlight())
	}
}

// TC-KLN-003: A failing (poison) record is reported but does not block its lane; revoked partitions are not committed.
func TestPoisonAndRevoke(t *testing.T) {
	var failed atomic.Int32
	block := make(chan struct{})
	e := New(context.Background(), 4, func(_ context.Context, r Record) error {
		if r.Key == "slow" {
			<-block
		}
		if r.Offset == 0 {
			return errors.New("poison")
		}
		return nil
	}, func(Record, error) { failed.Add(1) })
	a, b := TP{"t", 0}, TP{"t", 1}
	e.Submit(Record{TP: a, Offset: 0, Key: "k"})
	e.Submit(Record{TP: a, Offset: 1, Key: "k"})
	e.Submit(Record{TP: b, Offset: 5, Key: "slow"})
	time.Sleep(20 * time.Millisecond)
	if st := e.Stats(); st.InFlight != 1 || st.MaxLaneDepth < 1 {
		t.Fatalf("stats %+v", st)
	}
	e.Revoke(b)
	close(block)
	e.Wait()
	c := e.TakeCommits()
	if failed.Load() != 1 || c[a] != 2 {
		t.Fatalf("failed=%d commits=%v", failed.Load(), c)
	}
	if _, ok := c[b]; ok {
		t.Fatal("revoked partition committed")
	}
	if e.TakeCommits() != nil {
		t.Fatal("commits not cleared")
	}
}
