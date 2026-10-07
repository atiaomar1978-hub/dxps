package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"dxps/internal/bus"
	"dxps/internal/catalog"
	"dxps/internal/contract"
	"dxps/internal/planner"
	"dxps/internal/registry"
	"dxps/internal/store"
	"dxps/internal/tenant"
)

type ServiceConfig struct {
	Brokers   []string
	Instance  string
	StatsAddr string // loopback stats endpoint for the dashboard ("" = off)
}

type busPublisher struct {
	p *bus.Producer
	s *store.Store
}

func (b busPublisher) Publish(ctx context.Context, rows []*store.OutboxRow) error {
	return bus.FastPublish(ctx, b.p, b.s, rows)
}

// Refresh reloads tenants and the NE registry (ops role).
func Refresh(ctx context.Context, s *store.Store, tr *tenant.Registry, nr *registry.Registry) error {
	ts, err := s.LoadTenants(ctx)
	if err != nil {
		return err
	}
	nes, err := s.LoadNEs(ctx)
	if err != nil {
		return err
	}
	tr.Replace(ts)
	nr.Replace(nes)
	return nil
}

// New builds an orchestrator with loaded catalog and registries.
func New(ctx context.Context, s *store.Store, log *slog.Logger) (*Orchestrator, error) {
	cat, err := catalog.Load()
	if err != nil {
		return nil, err
	}
	tr, nr := tenant.NewRegistry(), registry.New()
	if err := Refresh(ctx, s, tr, nr); err != nil {
		return nil, err
	}
	return &Orchestrator{Store: s, Catalog: cat, Planner: &planner.Planner{Catalog: cat, Registry: nr},
		Tenants: tr, Registry: nr, Log: log}, nil
}

// withRetry retries transient failures (DB/Kafka) with backoff; permanent errors return at once.
func withRetry(ctx context.Context, f func() error) error {
	var err error
	for i := 0; i < 8; i++ {
		if err = f(); err == nil || errors.Is(err, ErrPermanent) || ctx.Err() != nil {
			return err
		}
		time.Sleep(time.Duration(50*(1<<i)) * time.Millisecond)
	}
	return err
}

// Run starts the BC and result consumers, the outbox relay, the retry forwarder and registry refresh.
func (o *Orchestrator) Run(ctx context.Context, cfg ServiceConfig) error {
	p, err := bus.NewProducer(cfg.Brokers, "orchestrator")
	if err != nil {
		return err
	}
	defer p.Close()
	o.Pub = busPublisher{p, o.Store}
	toDLQ := func(r *kgo.Record, err error) {
		o.log().Error("message to DLQ", "topic", r.Topic, "offset", r.Offset, "err", err)
		hs := map[string]string{contract.HdrTenant: bus.Header(r, contract.HdrTenant), "error": err.Error(), "source_topic": r.Topic}
		_ = p.Publish(context.Background(), bus.Record(contract.TopicDLQ, string(r.Key), r.Value, hs))
	}
	var tiers [4][]string
	for pr := contract.P0; pr <= contract.P3; pr++ {
		tiers[pr] = []string{contract.BCTopic(pr)}
	}
	bcc, err := bus.NewTieredConsumer(bus.ConsumerConfig{Brokers: cfg.Brokers, Group: "dxps-orchestrator-bc", Instance: cfg.Instance,
		TierTopics: tiers, Concurrency: 128, Quantum: o.Tenants.Quantum, Log: o.log(), OnError: toDLQ,
		Handle: func(ctx context.Context, r *kgo.Record) error {
			var bc contract.BusinessCommand
			if err := json.Unmarshal(r.Value, &bc); err != nil {
				return errors.Join(ErrPermanent, err)
			}
			return withRetry(ctx, func() error { return o.HandleBC(ctx, &bc, bus.Header(r, contract.HdrTenant), string(r.Key)) })
		}})
	if err != nil {
		return err
	}
	var rt [4][]string
	rt[contract.P1] = []string{contract.TopicTaskResult}
	rc, err := bus.NewTieredConsumer(bus.ConsumerConfig{Brokers: cfg.Brokers, Group: "dxps-orchestrator-result", Instance: cfg.Instance,
		TierTopics: rt, Concurrency: 128, Quantum: o.Tenants.Quantum, Log: o.log(), OnError: toDLQ,
		Handle: func(ctx context.Context, r *kgo.Record) error {
			var tr contract.TaskResult
			if err := json.Unmarshal(r.Value, &tr); err != nil {
				return errors.Join(ErrPermanent, err)
			}
			return withRetry(ctx, func() error { return o.HandleResult(ctx, &tr, bus.Header(r, contract.HdrTenant), string(r.Key)) })
		}})
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	start := func(name string, f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil && ctx.Err() == nil {
				o.log().Error(name+" stopped", "err", err)
			}
		}()
	}
	start("bc consumer", func() error { return bcc.Run(ctx) })
	start("result consumer", func() error { return rc.Run(ctx) })
	start("retry forwarder", func() error { return bus.RunRetryForwarder(ctx, cfg.Brokers, p, o.log()) })
	start("outbox relay", func() error { return o.relay(ctx, p) })
	start("refresh", func() error {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				if err := Refresh(ctx, o.Store, o.Tenants, o.Registry); err != nil {
					o.log().Warn("registry refresh", "err", err)
				}
			}
		}
	})
	if cfg.StatsAddr != "" {
		start("stats", func() error { return serveStats(ctx, cfg.StatsAddr, bcc, rc) })
	}
	wg.Wait()
	return nil
}

func (o *Orchestrator) relay(ctx context.Context, p *bus.Producer) error {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			n, err := o.Store.RelayBatch(ctx, 2*time.Second, 500, func(rows []*store.OutboxRow) error {
				return p.Publish(ctx, bus.OutboxRecords(rows)...)
			})
			if err != nil && ctx.Err() == nil {
				o.log().Warn("relay", "err", err)
			} else if n > 0 {
				o.log().Info("relay published", "rows", n)
			}
		}
	}
}

func serveStats(ctx context.Context, addr string, bcc, rc *bus.TieredConsumer) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || (host != "127.0.0.1" && host != "localhost") {
		return errors.New("stats endpoint must bind to loopback")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		bs, bl := bcc.Stats()
		rs, rl := rc.Stats()
		_ = json.NewEncoder(w).Encode(map[string]any{"bcScheduler": bs, "bcLanes": bl, "resultScheduler": rs, "resultLanes": rl})
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
