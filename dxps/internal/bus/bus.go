// Package bus wraps franz-go for DxPS: topic layout, tier-tuned idempotent producers, KIP-848
// tiered consumers feeding the weighted-fair scheduler and key-lane executor, EOS sessions, admin/lag.
package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"dxps/internal/contract"
	"dxps/internal/store"
)

type TopicSpec struct {
	Name       string
	Partitions int32
	Configs    map[string]*string
}

func ptr(s string) *string { return &s }

// Topics returns the DxPS topic layout (LLD 5.4 / 6.3.3), sized for a single local broker.
func Topics() []TopicSpec {
	base := map[string]*string{"min.insync.replicas": ptr("1"), "unclean.leader.election.enable": ptr("false"),
		"message.timestamp.type": ptr("CreateTime"), "compression.type": ptr("producer")}
	with := func(extra map[string]string) map[string]*string {
		m := map[string]*string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = ptr(v)
		}
		return m
	}
	var out []TopicSpec
	for p := contract.P0; p <= contract.P3; p++ {
		out = append(out, TopicSpec{contract.BCTopic(p), 6, with(map[string]string{"retention.ms": "604800000"})})
		for _, d := range contract.Domains {
			out = append(out, TopicSpec{contract.TaskTopic(d, p), 3, with(nil)})
		}
	}
	compact := map[string]string{"cleanup.policy": "compact", "min.compaction.lag.ms": "60000", "segment.ms": "3600000"}
	out = append(out,
		TopicSpec{contract.TopicTaskResult, 6, with(nil)},
		TopicSpec{contract.TopicRetry5s, 3, with(nil)},
		TopicSpec{contract.TopicRetry30s, 3, with(nil)},
		TopicSpec{contract.TopicRetry5m, 3, with(nil)},
		TopicSpec{contract.TopicDLQ, 3, with(map[string]string{"retention.ms": "2592000000"})},
		TopicSpec{contract.TopicStateOrder, 6, with(compact)},
		TopicSpec{contract.TopicStateNE, 1, with(compact)},
		TopicSpec{contract.TopicStateTenant, 1, with(compact)},
		TopicSpec{contract.TopicEvent, 6, with(nil)},
	)
	return out
}

func baseOpts(brokers []string, app string) []kgo.Opt {
	return []kgo.Opt{kgo.SeedBrokers(brokers...), kgo.ClientID(app), kgo.DialTimeout(5 * time.Second)}
}

// EnsureTopics creates missing topics; it returns the names created.
func EnsureTopics(ctx context.Context, brokers []string) ([]string, error) {
	cl, err := kgo.NewClient(baseOpts(brokers, "dxpsctl")...)
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	existing, err := adm.ListTopics(ctx)
	if err != nil {
		return nil, err
	}
	var created []string
	for _, t := range Topics() {
		if existing.Has(t.Name) {
			continue
		}
		resp, err := adm.CreateTopic(ctx, t.Partitions, 1, t.Configs, t.Name)
		if err != nil {
			return created, err
		}
		if resp.Err != nil {
			return created, fmt.Errorf("%s: %w", t.Name, resp.Err)
		}
		created = append(created, t.Name)
	}
	return created, nil
}

// ---------------------------------------------------------------- headers

func Headers(m map[string]string) []kgo.RecordHeader {
	h := make([]kgo.RecordHeader, 0, len(m))
	for k, v := range m {
		h = append(h, kgo.RecordHeader{Key: k, Value: []byte(v)})
	}
	return h
}

func Header(r *kgo.Record, k string) string {
	for _, h := range r.Headers {
		if h.Key == k {
			return string(h.Value)
		}
	}
	return ""
}

// ---------------------------------------------------------------- producer

// Producer holds two idempotent producers: "fast" for P0/P1 (lz4, no linger, 5 s delivery timeout)
// and "bulk" for P2/P3 (zstd, 10 ms linger, 120 s delivery timeout). acks=all, idempotence on (default).
type Producer struct {
	fast, bulk *kgo.Client
	app        string
}

func NewProducer(brokers []string, app string) (*Producer, error) {
	fast, err := kgo.NewClient(append(baseOpts(brokers, app+"-fast"),
		kgo.RequiredAcks(kgo.AllISRAcks()), kgo.ProducerLinger(0),
		kgo.ProducerBatchCompression(kgo.Lz4Compression()), kgo.ProducerBatchMaxBytes(64<<10),
		kgo.RecordDeliveryTimeout(5*time.Second), kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil)))...)
	if err != nil {
		return nil, err
	}
	bulk, err := kgo.NewClient(append(baseOpts(brokers, app+"-bulk"),
		kgo.RequiredAcks(kgo.AllISRAcks()), kgo.ProducerLinger(10*time.Millisecond),
		kgo.ProducerBatchCompression(kgo.ZstdCompression()), kgo.ProducerBatchMaxBytes(1<<20),
		kgo.RecordDeliveryTimeout(120*time.Second), kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil)))...)
	if err != nil {
		fast.Close()
		return nil, err
	}
	return &Producer{fast: fast, bulk: bulk, app: app}, nil
}

func (p *Producer) Close() {
	p.fast.Close()
	p.bulk.Close()
}

func (p *Producer) client(prio string) *kgo.Client {
	if prio == "p2" || prio == "p3" {
		return p.bulk
	}
	return p.fast
}

// Publish produces records synchronously (grouped by tier client).
func (p *Producer) Publish(ctx context.Context, recs ...*kgo.Record) error {
	var fast, bulk []*kgo.Record
	for _, r := range recs {
		r.Headers = append(r.Headers, kgo.RecordHeader{Key: contract.HdrProducer, Value: []byte(p.app)})
		if p.client(Header(r, contract.HdrPriority)) == p.bulk {
			bulk = append(bulk, r)
		} else {
			fast = append(fast, r)
		}
	}
	if len(fast) > 0 {
		if err := p.fast.ProduceSync(ctx, fast...).FirstErr(); err != nil {
			return err
		}
	}
	if len(bulk) > 0 {
		return p.bulk.ProduceSync(ctx, bulk...).FirstErr()
	}
	return nil
}

// PublishJSON marshals v and publishes it with DxPS headers.
func (p *Producer) PublishJSON(ctx context.Context, topic, key, tenantID, prio string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return p.Publish(ctx, Record(topic, key, b, map[string]string{contract.HdrTenant: tenantID, contract.HdrPriority: prio}))
}

func Record(topic, key string, value []byte, headers map[string]string) *kgo.Record {
	return &kgo.Record{Topic: topic, Key: []byte(key), Value: value, Headers: Headers(headers)}
}

// OutboxRecords converts outbox rows to Kafka records.
func OutboxRecords(rows []*store.OutboxRow) []*kgo.Record {
	out := make([]*kgo.Record, len(rows))
	for i, r := range rows {
		out[i] = Record(r.Topic, r.Key, r.Payload, r.Headers)
	}
	return out
}

// FastPublish publishes outbox rows right after commit and marks them published; on failure the relay
// publishes them later (at-least-once; consumers dedupe by message id).
func FastPublish(ctx context.Context, p *Producer, s *store.Store, rows []*store.OutboxRow) error {
	if len(rows) == 0 {
		return nil
	}
	if err := p.Publish(ctx, OutboxRecords(rows)...); err != nil {
		return err
	}
	return s.MarkPublished(ctx, rows)
}

// ---------------------------------------------------------------- admin

type TopicStat struct {
	Topic string `json:"topic"`
	End   int64  `json:"end"`
}

type GroupLag struct {
	Group string `json:"group"`
	Lag   int64  `json:"lag"`
	State string `json:"state"`
}

type Admin struct {
	cl  *kgo.Client
	adm *kadm.Client
}

func NewAdmin(brokers []string) (*Admin, error) {
	cl, err := kgo.NewClient(baseOpts(brokers, "dxps-admin")...)
	if err != nil {
		return nil, err
	}
	return &Admin{cl: cl, adm: kadm.NewClient(cl)}, nil
}

func (a *Admin) Close() { a.cl.Close() }

func (a *Admin) TopicEnds(ctx context.Context) ([]TopicStat, error) {
	var names []string
	for _, t := range Topics() {
		names = append(names, t.Name)
	}
	ends, err := a.adm.ListEndOffsets(ctx, names...)
	if err != nil {
		return nil, err
	}
	var out []TopicStat
	for _, n := range names {
		var sum int64
		ends.Each(func(o kadm.ListedOffset) {
			if o.Topic == n && o.Err == nil {
				sum += o.Offset
			}
		})
		out = append(out, TopicStat{Topic: n, End: sum})
	}
	return out, nil
}

func (a *Admin) Lags(ctx context.Context) ([]GroupLag, error) {
	groups, err := a.adm.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	lags, err := a.adm.Lag(ctx, groups.Groups()...)
	if err != nil {
		return nil, err
	}
	// KIP-848 groups are invisible to the classic describe (reported as Dead): ask the new API.
	var next []string
	for g, l := range lags {
		if l.State == "Dead" {
			next = append(next, g)
		}
	}
	states := map[string]string{}
	if len(next) > 0 {
		if cg, err := a.adm.DescribeConsumerGroups(ctx, next...); err == nil {
			for g, d := range cg {
				if d.Err == nil && d.State != "" {
					states[g] = d.State + " (848)"
				}
			}
		}
	}
	var out []GroupLag
	lags.Each(func(l kadm.DescribedGroupLag) {
		st := l.State
		if s, ok := states[l.Group]; ok {
			st = s
		}
		out = append(out, GroupLag{Group: l.Group, Lag: l.Lag.Total(), State: st})
	})
	return out, nil
}
