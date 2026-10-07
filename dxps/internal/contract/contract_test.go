package contract

import (
	"errors"
	"testing"
	"time"
)

// TC-CON-001: Priorities parse from TMF ("0".."3", "P2") and render as topic suffixes.
func TestPriority(t *testing.T) {
	for in, want := range map[string]Priority{"0": P0, "p1": P1, " P2 ": P2, "3": P3} {
		got, err := ParsePriority(in)
		if err != nil || got != want {
			t.Fatalf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "4", "p", "-1", "high", "12"} {
		if _, err := ParsePriority(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if P0.String() != "p0" || !P3.Valid() || Priority(4).Valid() {
		t.Fatal("priority helpers")
	}
}

// TC-CON-002: Topic naming and retry-ladder selection by delay.
func TestTopics(t *testing.T) {
	if BCTopic(P1) != "dxps.bc.p1" || TaskTopic("sba", P0) != "dxps.task.sba.p0" {
		t.Fatal("topic names")
	}
	for d, want := range map[time.Duration]string{time.Second: TopicRetry5s, 5 * time.Second: TopicRetry5s, 6 * time.Second: TopicRetry30s,
		30 * time.Second: TopicRetry30s, time.Minute: TopicRetry5m} {
		if RetryTopicFor(d) != want {
			t.Fatalf("%v", d)
		}
	}
	if !Atomic.Valid() || TxMode("X").Valid() || len(Domains) != 7 {
		t.Fatal("txmode/domains")
	}
}

// TC-CON-003: Header tenant, key prefix and payload tenant must agree (cross-tenant message injection).
func TestCheckTenant(t *testing.T) {
	k := Key("mvno-alpha", "supi:imsi-1")
	if TenantOfKey(k) != "mvno-alpha" || CheckTenant("mvno-alpha", k, "mvno-alpha") != nil {
		t.Fatal("valid record rejected")
	}
	for _, c := range [][3]string{{"host-mno", k, "mvno-alpha"}, {"mvno-alpha", Key("host-mno", "x"), "mvno-alpha"},
		{"mvno-alpha", k, "host-mno"}, {"", "", ""}, {"a b", "a b|x", "a b"}} {
		if !errors.Is(CheckTenant(c[0], c[1], c[2]), ErrTenantMismatch) {
			t.Fatalf("%v accepted", c)
		}
	}
}

// TC-CON-004: BusinessCommand validation and entity keys.
func TestValidate(t *testing.T) {
	b := BusinessCommand{Tenant: "t", MessageID: "m", OrderID: "o", BCID: "b", EntityKey: "supi:1", ExtraEntityKeys: []string{"msisdn:2"},
		CommandSpec: "X", Priority: P1, TxMode: Atomic}
	if b.Validate() != nil || len(b.EntityKeys()) != 2 {
		t.Fatal("valid bc rejected")
	}
	for _, mut := range []func(*BusinessCommand){func(c *BusinessCommand) { c.Tenant = "" }, func(c *BusinessCommand) { c.Priority = 9 },
		func(c *BusinessCommand) { c.TxMode = "" }, func(c *BusinessCommand) { c.EntityKey = "" }} {
		c := b
		mut(&c)
		if !errors.Is(c.Validate(), ErrInvalid) {
			t.Fatal("invalid bc accepted")
		}
	}
}
