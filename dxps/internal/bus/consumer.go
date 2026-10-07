package bus

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"

	"dxps/internal/contract"
	"dxps/internal/keylane"
	"dxps/internal/scheduler"
)

type Handler func(ctx context.Context, r *kgo.Record) error

type ConsumerConfig struct {
	Brokers     []string
	Group       string
	Instance    string      // static membership id prefix ("" = dynamic)
	TierTopics  [4][]string // topics per priority tier
	Concurrency int         // key-lane executor cap
	QueueCap    int         // scheduler capacity (backpressure)
	Quantum     func(string) int
	Handle      Handler
	OnError     func(*kgo.Record, error)
	Log         *slog.Logger
}

// TieredConsumer: one KIP-848 consumer per tier -> weighted-fair scheduler (tiers + tenant DRR) ->
// key-lane executor -> contiguous offset commits.
type TieredConsumer struct {
	cfg     ConsumerConfig
	clients [4]*kgo.Client
	tierOf  map[string]int
	Sched   *scheduler.Scheduler[*kgo.Record]
	Exec    *keylane.Executor
	mu      sync.Mutex
}

func NewTieredConsumer(cfg ConsumerConfig) (*TieredConsumer, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 256
	}
	if cfg.QueueCap == 0 {
		cfg.QueueCap = 4096
	}
	tc := &TieredConsumer{cfg: cfg, tierOf: map[string]int{}}
	tc.Sched = scheduler.New[*kgo.Record](cfg.QueueCap, cfg.Quantum)
	for t, topics := range cfg.TierTopics {
		if len(topics) == 0 {
			continue
		}
		for _, n := range topics {
			tc.tierOf[n] = t
		}
		wait, minBytes := 10*time.Millisecond, int32(1) // franz-go minimum
		if t == int(contract.P3) {
			wait, minBytes = 50*time.Millisecond, 64<<10
		}
		tier := t
		opts := append(baseOpts(cfg.Brokers, cfg.Group+"-"+contract.Priority(t).String()),
			kgo.ConsumerGroup(cfg.Group+"."+contract.Priority(t).String()),
			kgo.ConsumeTopics(topics...),
			kgo.DisableAutoCommit(),
			kgo.FetchIsolationLevel(kgo.ReadCommitted()),
			kgo.ServerSideBalancer(), kgo.Balancers(kgo.CooperativeStickyBalancer()),
			kgo.FetchMaxWait(wait), kgo.FetchMinBytes(minBytes),
			kgo.FetchMaxPartitionBytes(1<<20),
			kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
			kgo.OnPartitionsRevoked(func(ctx context.Context, cl *kgo.Client, m map[string][]int32) {
				tc.commit(ctx)
				tc.revoke(m)
			}),
			kgo.OnPartitionsLost(func(_ context.Context, _ *kgo.Client, m map[string][]int32) { tc.revoke(m) }),
		)
		if cfg.Instance != "" {
			opts = append(opts, kgo.InstanceID(cfg.Instance+"-"+contract.Priority(tier).String()))
		}
		cl, err := kgo.NewClient(opts...)
		if err != nil {
			tc.Close()
			return nil, err
		}
		tc.clients[t] = cl
	}
	return tc, nil
}

func (tc *TieredConsumer) exec() *keylane.Executor {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.Exec
}

// Stats returns scheduler and executor statistics (safe for concurrent use).
func (tc *TieredConsumer) Stats() (scheduler.Stats, keylane.Stats) {
	var ks keylane.Stats
	if e := tc.exec(); e != nil {
		ks = e.Stats()
	}
	return tc.Sched.Stats(), ks
}

func (tc *TieredConsumer) revoke(m map[string][]int32) {
	ex := tc.exec()
	if ex == nil {
		return
	}
	var tps []keylane.TP
	for t, ps := range m {
		for _, p := range ps {
			tps = append(tps, keylane.TP{Topic: t, Partition: p})
		}
	}
	ex.Revoke(tps...)
}

func (tc *TieredConsumer) commit(ctx context.Context) {
	ex := tc.exec()
	if ex == nil {
		return
	}
	commits := ex.TakeCommits()
	if len(commits) == 0 {
		return
	}
	per := map[int]map[string]map[int32]kgo.EpochOffset{}
	for tp, off := range commits {
		t := tc.tierOf[tp.Topic]
		if per[t] == nil {
			per[t] = map[string]map[int32]kgo.EpochOffset{}
		}
		if per[t][tp.Topic] == nil {
			per[t][tp.Topic] = map[int32]kgo.EpochOffset{}
		}
		per[t][tp.Topic][tp.Partition] = kgo.EpochOffset{Epoch: -1, Offset: off}
	}
	for t, offs := range per {
		if cl := tc.clients[t]; cl != nil {
			cl.CommitOffsetsSync(ctx, offs, func(_ *kgo.Client, _ *kmsg.OffsetCommitRequest, _ *kmsg.OffsetCommitResponse, err error) {
				if err != nil {
					tc.cfg.Log.Warn("commit failed", "err", err)
				}
			})
		}
	}
}

// Run consumes until ctx is cancelled.
func (tc *TieredConsumer) Run(ctx context.Context) error {
	ex := keylane.New(ctx, tc.cfg.Concurrency, func(ctx context.Context, r keylane.Record) error {
		return tc.cfg.Handle(ctx, r.Value.(*kgo.Record))
	}, func(r keylane.Record, err error) {
		if tc.cfg.OnError != nil {
			tc.cfg.OnError(r.Value.(*kgo.Record), err)
		}
	})
	tc.mu.Lock()
	tc.Exec = ex
	tc.mu.Unlock()
	var wg sync.WaitGroup
	for t, cl := range tc.clients {
		if cl == nil {
			continue
		}
		wg.Add(1)
		go func(t int, cl *kgo.Client) {
			defer wg.Done()
			for ctx.Err() == nil {
				fs := cl.PollRecords(ctx, 500)
				fs.EachError(func(topic string, p int32, err error) {
					if ctx.Err() == nil {
						tc.cfg.Log.Warn("fetch error", "topic", topic, "partition", p, "err", err)
					}
				})
				fs.EachRecord(func(r *kgo.Record) {
					ten := Header(r, contract.HdrTenant)
					if ten == "" {
						ten = contract.TenantOfKey(string(r.Key))
					}
					_ = tc.Sched.Push(ctx, scheduler.Item[*kgo.Record]{Tenant: ten, Tier: contract.Priority(t), V: r})
				})
			}
		}(t, cl)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			it, err := tc.Sched.Next(ctx)
			if err != nil {
				return
			}
			r := it.V
			ex.Submit(keylane.Record{TP: keylane.TP{Topic: r.Topic, Partition: r.Partition}, Offset: r.Offset, Key: string(r.Key), Value: r})
		}
	}()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			tc.commit(ctx)
		case <-ctx.Done():
			wg.Wait()
			ex.Wait()
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			tc.commit(cctx)
			cancel()
			tc.Close()
			return nil
		}
	}
}

func (tc *TieredConsumer) Close() {
	for _, cl := range tc.clients {
		if cl != nil {
			cl.Close()
		}
	}
}

// ---------------------------------------------------------------- EOS session (adapters)

type EOSConfig struct {
	Brokers       []string
	Group         string
	TransactionID string
	Topics        []string
	Concurrency   int
	MaxPoll       int
	// Process handles one record and returns the records to produce atomically with the offset commit.
	Process func(ctx context.Context, r *kgo.Record) []*kgo.Record
	Log     *slog.Logger
}

// RunEOS runs a consume-process-produce loop in Kafka transactions (KIP-98 / KIP-447): results are
// produced and offsets committed atomically. Records of the same key in a batch run serially.
func RunEOS(ctx context.Context, cfg EOSConfig) error {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.MaxPoll == 0 {
		cfg.MaxPoll = 256
	}
	sess, err := kgo.NewGroupTransactSession(append(baseOpts(cfg.Brokers, cfg.TransactionID),
		kgo.TransactionalID(cfg.TransactionID), kgo.TransactionTimeout(30*time.Second),
		kgo.ConsumerGroup(cfg.Group), kgo.ConsumeTopics(cfg.Topics...),
		kgo.FetchIsolationLevel(kgo.ReadCommitted()), kgo.RequireStableFetchOffsets(),
		kgo.Balancers(kgo.CooperativeStickyBalancer()), kgo.FetchMaxWait(10*time.Millisecond),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.RequiredAcks(kgo.AllISRAcks()), kgo.ProducerLinger(0),
		kgo.ProducerBatchCompression(kgo.Lz4Compression()))...)
	if err != nil {
		return err
	}
	defer sess.Close()
	sem := make(chan struct{}, max(cfg.Concurrency, 1))
	for ctx.Err() == nil {
		fs := sess.PollRecords(ctx, cfg.MaxPoll)
		if ctx.Err() != nil {
			break
		}
		fs.EachError(func(t string, p int32, err error) { cfg.Log.Warn("eos fetch", "topic", t, "err", err) })
		if fs.NumRecords() == 0 {
			continue
		}
		lanes := map[string][]*kgo.Record{}
		var order []string
		fs.EachRecord(func(r *kgo.Record) {
			k := string(r.Key)
			if _, ok := lanes[k]; !ok {
				order = append(order, k)
			}
			lanes[k] = append(lanes[k], r)
		})
		outs := make([][]*kgo.Record, len(order))
		var wg sync.WaitGroup
		for i, k := range order {
			wg.Add(1)
			go func(i int, recs []*kgo.Record) {
				defer wg.Done()
				for _, r := range recs {
					sem <- struct{}{}
					outs[i] = append(outs[i], cfg.Process(ctx, r)...)
					<-sem
				}
			}(i, lanes[k])
		}
		wg.Wait()
		if err := sess.Begin(); err != nil {
			cfg.Log.Error("eos begin", "err", err)
			continue
		}
		var perr error
		var pmu sync.Mutex
		for _, o := range outs {
			for _, r := range o {
				sess.Produce(ctx, r, func(_ *kgo.Record, err error) {
					if err != nil {
						pmu.Lock()
						perr = err
						pmu.Unlock()
					}
				})
			}
		}
		commit := kgo.TryCommit
		if err := sess.Client().Flush(ctx); err != nil || perr != nil {
			commit = kgo.TryAbort
		}
		if ok, err := sess.End(ctx, commit); err != nil || !ok {
			cfg.Log.Warn("eos transaction not committed; batch will be reprocessed (idempotent NE calls)", "err", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------- retry ladder forwarder

// RunRetryForwarder consumes dxps.retry.* and republishes each record to its target topic once its
// not_before time has passed (records in one ladder topic share the same delay class, so FIFO holds).
func RunRetryForwarder(ctx context.Context, brokers []string, p *Producer, log *slog.Logger) error {
	cl, err := kgo.NewClient(append(baseOpts(brokers, "dxps-retry"),
		kgo.ConsumerGroup("dxps-retry-forwarder"), kgo.ConsumeTopics(contract.RetryTopics...),
		kgo.DisableAutoCommit(), kgo.FetchIsolationLevel(kgo.ReadCommitted()),
		kgo.ServerSideBalancer(), kgo.Balancers(kgo.CooperativeStickyBalancer()),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))...)
	if err != nil {
		return err
	}
	defer cl.Close()
	for ctx.Err() == nil {
		fs := cl.PollRecords(ctx, 200)
		var ok []*kgo.Record
		fs.EachRecord(func(r *kgo.Record) {
			if nb, err := time.Parse(time.RFC3339Nano, Header(r, contract.HdrNotBefore)); err == nil {
				if d := time.Until(nb); d > 0 {
					select {
					case <-time.After(d):
					case <-ctx.Done():
						return
					}
				}
			}
			target := Header(r, contract.HdrTarget)
			if target == "" {
				target = contract.TopicDLQ
			}
			var hs []kgo.RecordHeader
			for _, h := range r.Headers {
				if h.Key != contract.HdrNotBefore && h.Key != contract.HdrTarget && h.Key != contract.HdrProducer {
					hs = append(hs, h)
				}
			}
			if err := p.Publish(ctx, &kgo.Record{Topic: target, Key: r.Key, Value: r.Value, Headers: hs}); err != nil {
				log.Error("retry forward failed", "err", err)
				return
			}
			ok = append(ok, r)
		})
		if len(ok) > 0 {
			if err := cl.CommitRecords(ctx, ok...); err != nil {
				log.Warn("retry commit", "err", err)
			}
		}
	}
	return nil
}
