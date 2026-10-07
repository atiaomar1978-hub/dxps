package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"dxps/internal/contract"
	"dxps/internal/ids"
	"dxps/internal/store"
	"dxps/internal/testenv"
)

// TC-BUS-001: topic layout: 41 topics (4 BC tiers, 28 task topics, result, 3 retry, DLQ, 3 compacted state, event).
func TestTopics(t *testing.T) {
	ts := Topics()
	if len(ts) != 41 {
		t.Fatal(len(ts))
	}
	byName := map[string]TopicSpec{}
	for _, s := range ts {
		byName[s.Name] = s
		if *s.Configs["unclean.leader.election.enable"] != "false" {
			t.Fatal(s.Name, "unclean election")
		}
	}
	if byName["dxps.bc.p0"].Partitions != 6 || byName["dxps.task.sba.p3"].Partitions != 3 || *byName[contract.TopicStateOrder].Configs["cleanup.policy"] != "compact" ||
		*byName[contract.TopicDLQ].Configs["retention.ms"] != "2592000000" {
		t.Fatal("layout")
	}
}

// TC-BUS-002: headers, records and outbox conversion; tier routing of the producer.
func TestRecordsAndHeaders(t *testing.T) {
	r := Record("t", "k", []byte("v"), map[string]string{"a": "1", "b": "2"})
	if Header(r, "a") != "1" || Header(r, "zz") != "" || string(r.Key) != "k" {
		t.Fatal(r)
	}
	rs := OutboxRecords([]*store.OutboxRow{{Topic: "x", Key: "y", Payload: []byte("p"), Headers: map[string]string{"h": "v"}}})
	if rs[0].Topic != "x" || Header(rs[0], "h") != "v" {
		t.Fatal(rs)
	}
	p := &Producer{fast: &kgo.Client{}, bulk: &kgo.Client{}}
	if p.client("p0") != p.fast || p.client("p1") != p.fast || p.client("p2") != p.bulk || p.client("p3") != p.bulk || p.client("") != p.fast {
		t.Fatal("tier routing")
	}
	if err := FastPublish(context.Background(), nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- integration (Kafka)

// uniq returns random characters for names (the head of a UUIDv7 is a timestamp shared by concurrent tests).
func uniq() string { s := strings.ReplaceAll(ids.NewString(), "-", ""); return s[len(s)-12:] }

func tempTopics(t *testing.T, brokers []string, n int, parts int32) []string {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatal(err)
	}
	adm := kadm.NewClient(cl)
	base := "dxps-test." + strings.ReplaceAll(uniq(), "-", "")
	var names []string
	for i := 0; i < n; i++ {
		names = append(names, fmt.Sprintf("%s.%d", base, i))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Short retention instead of deletion: deleting topics on Windows fails the broker's log dir
	// (segment rename of memory-mapped files), so test topics expire on their own.
	cfg := map[string]*string{"retention.ms": ptr("3600000")}
	if _, err := adm.CreateTopics(ctx, parts, 1, cfg, names...); err != nil {
		t.Fatal(err)
	}
	// CreateTopics returns before the partitions have leaders; producing earlier can exceed the 5 s
	// delivery timeout of the fast producer on a broker with many partitions.
	for !topicsReady(ctx, adm, names, parts) {
		if ctx.Err() != nil {
			t.Fatal("temp topics have no leaders:", names)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Cleanup(cl.Close)
	return names
}

func topicsReady(ctx context.Context, adm *kadm.Client, names []string, parts int32) bool {
	md, err := adm.Metadata(ctx, names...)
	if err != nil {
		return false
	}
	for _, n := range names {
		td, ok := md.Topics[n]
		if !ok || td.Err != nil || int32(len(td.Partitions)) != parts {
			return false
		}
		for _, p := range td.Partitions {
			if p.Err != nil || p.Leader < 0 {
				return false
			}
		}
	}
	return true
}

func readAll(t *testing.T, brokers []string, topic string, want int, timeout time.Duration) []*kgo.Record {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumeTopics(topic), kgo.FetchIsolationLevel(kgo.ReadCommitted()),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var out []*kgo.Record
	for len(out) < want && ctx.Err() == nil {
		cl.PollFetches(ctx).EachRecord(func(r *kgo.Record) { out = append(out, r) })
	}
	return out
}

// TC-BUS-003 (Kafka): idempotent tier producers publish P0/P1 (fast) and P2/P3 (bulk) with the producer header.
func TestProducerIntegration(t *testing.T) {
	brokers := testenv.Kafka(t)
	topic := tempTopics(t, brokers, 1, 3)[0]
	p, err := NewProducer(brokers, "bus-test")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := context.Background()
	for i, prio := range []string{"p0", "p1", "p2", "p3"} {
		if err := p.PublishJSON(ctx, topic, fmt.Sprintf("mvno-alpha|k%d", i), "mvno-alpha", prio, map[string]int{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.PublishJSON(ctx, topic, "k", "t", "p1", func() {}); err == nil {
		t.Fatal("unmarshalable value accepted")
	}
	got := readAll(t, brokers, topic, 4, 15*time.Second)
	if len(got) != 4 {
		t.Fatal(len(got))
	}
	for _, r := range got {
		if Header(r, contract.HdrProducer) != "bus-test" || Header(r, contract.HdrTenant) != "mvno-alpha" {
			t.Fatal(r.Headers)
		}
	}
}

// TC-BUS-004 (Kafka): tiered KIP-848 consumer delivers every record once, keeps per-key order, reports handler errors
// via OnError and commits offsets (a restarted consumer in the same group does not see committed records again).
func TestTieredConsumerIntegration(t *testing.T) {
	brokers := testenv.Kafka(t)
	topics := tempTopics(t, brokers, 2, 3)
	p, err := NewProducer(brokers, "bus-test")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := context.Background()
	const perKey = 20
	var recs []*kgo.Record
	for _, ten := range []string{"mvno-alpha", "mvno-beta"} {
		for i := 0; i < perKey; i++ {
			topic, prio := topics[0], "p0"
			if ten == "mvno-beta" {
				topic, prio = topics[1], "p2"
			}
			recs = append(recs, Record(topic, ten+"|sub-1", []byte(fmt.Sprint(i)), map[string]string{contract.HdrTenant: ten, contract.HdrPriority: prio}))
		}
	}
	if err := p.Publish(ctx, recs...); err != nil {
		t.Fatal(err)
	}
	group := "dxps-test-" + uniq()
	var mu sync.Mutex
	seen := map[string][]string{}
	var errs int
	run := func(handle Handler) {
		var tiers [4][]string
		tiers[0], tiers[2] = []string{topics[0]}, []string{topics[1]}
		tc, err := NewTieredConsumer(ConsumerConfig{Brokers: brokers, Group: group, TierTopics: tiers, Handle: handle,
			OnError: func(*kgo.Record, error) { mu.Lock(); errs++; mu.Unlock() }})
		if err != nil {
			t.Fatal(err)
		}
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan error)
		go func() { done <- tc.Run(cctx) }()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := len(seen["mvno-alpha"]) + len(seen["mvno-beta"]) + errs
			mu.Unlock()
			if n >= 2*perKey {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(300 * time.Millisecond) // let the commit ticker run
		tc.Stats()
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	run(func(_ context.Context, r *kgo.Record) error {
		mu.Lock()
		defer mu.Unlock()
		ten := Header(r, contract.HdrTenant)
		if ten == "mvno-beta" && string(r.Value) == "7" {
			return errors.New("poison")
		}
		seen[ten] = append(seen[ten], string(r.Value))
		return nil
	})
	if len(seen["mvno-alpha"]) != perKey || len(seen["mvno-beta"]) != perKey-1 || errs != 1 {
		t.Fatal(len(seen["mvno-alpha"]), len(seen["mvno-beta"]), errs)
	}
	for i, v := range seen["mvno-alpha"] {
		if v != fmt.Sprint(i) {
			t.Fatal("per-key order violated", seen["mvno-alpha"])
		}
	}
	// Restart: committed records must not be redelivered.
	var again int
	var tiers [4][]string
	tiers[0], tiers[2] = []string{topics[0]}, []string{topics[1]}
	tc, err := NewTieredConsumer(ConsumerConfig{Brokers: brokers, Group: group, TierTopics: tiers,
		Handle: func(context.Context, *kgo.Record) error { mu.Lock(); again++; mu.Unlock(); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	_ = tc.Run(cctx)
	if again != 0 {
		t.Fatal("redelivered after commit:", again)
	}
}

// TC-BUS-005 (Kafka EOS): consume-process-produce in transactions: every input yields exactly one output visible to
// read_committed consumers; restarting the session does not reprocess committed input.
func TestEOSIntegration(t *testing.T) {
	brokers := testenv.Kafka(t)
	topics := tempTopics(t, brokers, 2, 3)
	in, out := topics[0], topics[1]
	p, err := NewProducer(brokers, "bus-test")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := context.Background()
	const n = 30
	var recs []*kgo.Record
	for i := 0; i < n; i++ {
		recs = append(recs, Record(in, fmt.Sprintf("t|k%d", i%5), []byte(fmt.Sprint(i)), map[string]string{contract.HdrPriority: "p1"}))
	}
	if err := p.Publish(ctx, recs...); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	processed := 0
	group, txid := "dxps-test-eos-"+uniq(), "dxps-test-tx-"+uniq()
	runEOS := func(d time.Duration) {
		cctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		err := RunEOS(cctx, EOSConfig{Brokers: brokers, Group: group, TransactionID: txid, Topics: []string{in}, Concurrency: 4,
			Process: func(_ context.Context, r *kgo.Record) []*kgo.Record {
				mu.Lock()
				processed++
				mu.Unlock()
				return []*kgo.Record{Record(out, string(r.Key), append([]byte("out-"), r.Value...), nil)}
			}})
		if err != nil {
			t.Fatal(err)
		}
	}
	runEOS(12 * time.Second)
	got := readAll(t, brokers, out, n, 15*time.Second)
	vals := map[string]bool{}
	for _, r := range got {
		vals[string(r.Value)] = true
	}
	if len(got) != n || len(vals) != n {
		t.Fatal("outputs", len(got), len(vals))
	}
	before := processed
	runEOS(6 * time.Second)
	if processed != before {
		t.Fatal("committed input reprocessed", processed-before)
	}
}

// TC-BUS-006 (Kafka): retry forwarder holds a record until not_before, strips ladder headers and republishes it to
// its target topic; records without a target go to the DLQ topic.
func TestRetryForwarderIntegration(t *testing.T) {
	brokers := testenv.Kafka(t)
	target := tempTopics(t, brokers, 1, 1)[0]
	p, err := NewProducer(brokers, "bus-test")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunRetryForwarder(ctx, brokers, p, slog.Default())
	nb := time.Now().Add(1500 * time.Millisecond)
	msg := ids.NewString()
	err = p.Publish(ctx, Record(contract.TopicRetry5s, "zz-test|k", []byte(msg), map[string]string{
		contract.HdrTenant: "zz-test", contract.HdrTarget: target, contract.HdrNotBefore: nb.UTC().Format(time.RFC3339Nano), contract.HdrPriority: "p1"}))
	if err != nil {
		t.Fatal(err)
	}
	got := readAll(t, brokers, target, 1, 30*time.Second)
	if len(got) != 1 || string(got[0].Value) != msg {
		t.Fatal(len(got))
	}
	if got[0].Timestamp.Before(nb.Add(-50 * time.Millisecond)) {
		t.Fatal("forwarded before not_before", got[0].Timestamp, nb)
	}
	if Header(got[0], contract.HdrTarget) != "" || Header(got[0], contract.HdrNotBefore) != "" || Header(got[0], contract.HdrTenant) != "zz-test" {
		t.Fatal(got[0].Headers)
	}
}

// TC-BUS-007 (Kafka): admin: topic creation is idempotent; end offsets cover all topics; group lag is reported.
func TestAdminIntegration(t *testing.T) {
	brokers := testenv.Kafka(t)
	ctx := context.Background()
	created, err := EnsureTopics(ctx, brokers)
	if err != nil || len(created) != 0 {
		t.Fatal(created, err)
	}
	a, err := NewAdmin(brokers)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ends, err := a.TopicEnds(ctx)
	if err != nil || len(ends) != 41 {
		t.Fatal(len(ends), err)
	}
	lags, err := a.Lags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(lags))
	for _, l := range lags {
		names = append(names, l.Group)
	}
	sort.Strings(names)
	t.Log("groups:", names)
}
