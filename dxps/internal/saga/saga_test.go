package saga

import (
	"errors"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"dxps/internal/contract"
)

// TC-SAG-001: NE responses are classified: 2xx success, 408/429/5xx(gateway) retryable, other 4xx/501 failed, transport errors retryable.
func TestClassify(t *testing.T) {
	cases := map[int]contract.Outcome{200: contract.Succeeded, 201: contract.Succeeded, 204: contract.Succeeded,
		408: contract.Retryable, 429: contract.Retryable, 500: contract.Retryable, 502: contract.Retryable, 503: contract.Retryable, 504: contract.Retryable,
		400: contract.Failed, 401: contract.Failed, 403: contract.Failed, 404: contract.Failed, 409: contract.Failed, 501: contract.Failed, 302: contract.Failed}
	for st, want := range cases {
		if got := Classify(st, nil); got != want {
			t.Fatalf("%d: %s", st, got)
		}
	}
	if Classify(0, errors.New("reset")) != contract.Retryable {
		t.Fatal("transport error")
	}
}

// TC-SAG-002: Backoff is exponential with full jitter, floored at 50 ms and capped at 5 min; Retry-After wins when larger.
func TestBackoff(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for a := -1; a < 40; a++ {
		d := Backoff(a, r)
		ceil := BaseBackoff << min(max(a, 0), 20)
		if ceil > MaxBackoff {
			ceil = MaxBackoff
		}
		if d < 50*time.Millisecond || d > max(ceil, 50*time.Millisecond) {
			t.Fatalf("attempt %d: %v", a, d)
		}
	}
	if d := NextDelay(0, 10*time.Second, r); d != 10*time.Second {
		t.Fatalf("retry-after ignored: %v", d)
	}
	if d := NextDelay(0, time.Hour, nil); d != MaxBackoff {
		t.Fatalf("retry-after not capped: %v", d)
	}
}

// TC-SAG-003: Retries stop at the attempt budget or the order deadline.
func TestExhausted(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Second)
	if Exhausted(0, 3, nil, now) || !Exhausted(2, 3, nil, now) || Exhausted(6, 0, nil, now) || !Exhausted(7, 0, nil, now) || !Exhausted(0, 3, &past, now) {
		t.Fatal("exhaustion rules")
	}
}

// TC-SAG-004: Compensation runs in reverse topological order (dependants first), skipping
// failed/non-compensable tasks; nothing is compensated after a succeeded pivot.
func TestCompensationOrder(t *testing.T) {
	// CreateSubscriber5G: auth -> (am, smf, policy) -> ocs(pivot)
	nodes := []Node{
		{ID: "auth", Seq: 0, Succeeded: true, HasComp: true},
		{ID: "am", Seq: 1, DependsOn: []string{"auth"}, Succeeded: true, HasComp: true},
		{ID: "smf", Seq: 2, DependsOn: []string{"auth"}, Succeeded: true, HasComp: true},
		{ID: "policy", Seq: 3, DependsOn: []string{"auth"}, Succeeded: false, HasComp: true},
		{ID: "ocs", Seq: 4, DependsOn: []string{"am", "smf"}, Succeeded: false, HasComp: true, Pivot: true},
		{ID: "noop", Seq: 5, Succeeded: true, HasComp: false},
	}
	if got := CompensationOrder(nodes); !reflect.DeepEqual(got, []string{"smf", "am", "auth"}) {
		t.Fatalf("order %v", got)
	}
	nodes[4].Succeeded = true
	if got := CompensationOrder(nodes); got != nil {
		t.Fatalf("compensated after pivot: %v", got)
	}
	cyc := []Node{{ID: "a", DependsOn: []string{"b"}, Succeeded: true, HasComp: true}, {ID: "b", DependsOn: []string{"a"}, Succeeded: true, HasComp: true}}
	if len(CompensationOrder(cyc)) != 2 {
		t.Fatal("cycle not handled")
	}
}

// TC-SAG-005: TMF641 order state is derived from the states of its business commands.
func TestOrderState(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, Acknowledged}, {[]string{Completed, "inProgress"}, InProgress}, {[]string{Completed, Completed}, Completed},
		{[]string{Completed, Failed}, Partial}, {[]string{Failed, Failed}, Failed}, {[]string{Rejected}, Rejected},
		{[]string{Cancelled, Cancelled}, Cancelled}, {[]string{Cancelled, Failed}, Failed}, {[]string{"pending"}, InProgress},
	}
	for _, c := range cases {
		if got := OrderState(c.in); got != c.want {
			t.Fatalf("%v: %s want %s", c.in, got, c.want)
		}
	}
	for s, want := range map[string]bool{Completed: true, Failed: true, Partial: true, Cancelled: true, Rejected: true, InProgress: false, Held: false, Pending: false} {
		if Terminal(s) != want {
			t.Fatal(s)
		}
	}
}
