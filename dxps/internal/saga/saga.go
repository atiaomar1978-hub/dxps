// Package saga holds the transaction-management rules: outcome classification, retry backoff,
// compensation ordering and order state derivation (TMF641 states).
package saga

import (
	"math/rand/v2"
	"sort"
	"time"

	"dxps/internal/contract"
)

const (
	DefaultMaxAttempts = 8
	BaseBackoff        = 200 * time.Millisecond
	MaxBackoff         = 5 * time.Minute
)

// Classify maps an NE response (HTTP status) or transport error to an outcome (LLD 6.8).
func Classify(status int, err error) contract.Outcome {
	if err != nil { // connect / timeout / reset: the NE may not have seen the call
		return contract.Retryable
	}
	switch {
	case status >= 200 && status < 300:
		return contract.Succeeded
	case status == 408, status == 429, status == 500, status == 502, status == 503, status == 504:
		return contract.Retryable
	default:
		return contract.Failed
	}
}

// Backoff returns exponential backoff with full jitter: rand[0, min(cap, base*2^attempt)).
func Backoff(attempt int, r *rand.Rand) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	ceil := BaseBackoff << min(attempt, 20)
	if ceil > MaxBackoff || ceil <= 0 {
		ceil = MaxBackoff
	}
	var f float64
	if r != nil {
		f = r.Float64()
	} else {
		f = rand.Float64()
	}
	d := time.Duration(f * float64(ceil))
	if d < 50*time.Millisecond {
		d = 50 * time.Millisecond
	}
	return d
}

// NextDelay honours Retry-After when larger than the computed backoff.
func NextDelay(attempt int, retryAfter time.Duration, r *rand.Rand) time.Duration {
	d := Backoff(attempt, r)
	if retryAfter > d {
		d = min(retryAfter, MaxBackoff)
	}
	return d
}

// Exhausted reports whether no further attempt is allowed.
func Exhausted(attempt, maxAttempts int, deadline *time.Time, now time.Time) bool {
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	return attempt+1 >= maxAttempts || (deadline != nil && now.After(*deadline))
}

// Node is a task in the plan DAG for compensation ordering.
type Node struct {
	ID        string
	DependsOn []string
	Seq       int // creation order (tie-break)
	Succeeded bool
	Pivot     bool
	HasComp   bool
}

// CompensationOrder returns the succeeded, compensable tasks in reverse topological order
// (dependants first). Returns nil if a pivot task succeeded (no compensation after the pivot).
func CompensationOrder(nodes []Node) []string {
	for _, n := range nodes {
		if n.Pivot && n.Succeeded {
			return nil
		}
	}
	idx := map[string]Node{}
	for _, n := range nodes {
		idx[n.ID] = n
	}
	// depth = longest path from a root; compensate deepest first.
	depth := map[string]int{}
	var d func(id string, seen map[string]bool) int
	d = func(id string, seen map[string]bool) int {
		if v, ok := depth[id]; ok {
			return v
		}
		if seen[id] {
			return 0
		}
		seen[id] = true
		best := 0
		for _, p := range idx[id].DependsOn {
			if _, ok := idx[p]; ok {
				best = max(best, d(p, seen)+1)
			}
		}
		depth[id] = best
		return best
	}
	var out []Node
	for _, n := range nodes {
		d(n.ID, map[string]bool{})
		if n.Succeeded && n.HasComp {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if depth[out[i].ID] != depth[out[j].ID] {
			return depth[out[i].ID] > depth[out[j].ID]
		}
		return out[i].Seq > out[j].Seq
	})
	ids := make([]string, len(out))
	for i, n := range out {
		ids[i] = n.ID
	}
	return ids
}

// TMF641 ServiceOrder states.
const (
	Acknowledged = "acknowledged"
	InProgress   = "inProgress"
	Held         = "held"
	Pending      = "pending"
	Completed    = "completed"
	Failed       = "failed"
	Partial      = "partial"
	Cancelled    = "cancelled"
	Rejected     = "rejected"
)

func Terminal(state string) bool {
	switch state {
	case Completed, Failed, Partial, Cancelled, Rejected:
		return true
	}
	return false
}

// OrderState derives the order state from its business command states.
func OrderState(bcStates []string) string {
	if len(bcStates) == 0 {
		return Acknowledged
	}
	var done, failed, active, cancelled, rejected int
	for _, s := range bcStates {
		switch s {
		case Completed:
			done++
		case Failed:
			failed++
		case Cancelled:
			cancelled++
		case Rejected:
			rejected++
		default:
			active++
		}
	}
	n := len(bcStates)
	switch {
	case active > 0:
		return InProgress
	case done == n:
		return Completed
	case rejected == n:
		return Rejected
	case cancelled == n:
		return Cancelled
	case done > 0:
		return Partial
	default:
		return Failed
	}
}
