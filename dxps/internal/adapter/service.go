package adapter

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
	"dxps/internal/contract"
)

type ServiceConfig struct {
	Brokers   []string
	Domains   []string
	Instance  string
	StatsAddr string
	Log       *slog.Logger
	Refresh   func(context.Context) error // reload NE registry
}

var tierConcurrency = [4]int{64, 64, 32, 16}

// Process handles one task record and returns the TaskResult record (or a DLQ record for invalid input).
func (a *Adapter) Process(ctx context.Context, r *kgo.Record) []*kgo.Record {
	var t contract.NeTask
	if err := json.Unmarshal(r.Value, &t); err != nil {
		return []*kgo.Record{bus.Record(contract.TopicDLQ, string(r.Key), r.Value, map[string]string{"error": "decode: " + err.Error()})}
	}
	if err := contract.CheckTenant(bus.Header(r, contract.HdrTenant), string(r.Key), t.Tenant); err != nil {
		return []*kgo.Record{bus.Record(contract.TopicDLQ, string(r.Key), r.Value, map[string]string{"error": err.Error()})}
	}
	res := a.Execute(ctx, &t)
	b, _ := json.Marshal(res)
	return []*kgo.Record{bus.Record(contract.TopicTaskResult, contract.Key(t.Tenant, t.OrderID), b, map[string]string{
		contract.HdrTenant: t.Tenant, contract.HdrPriority: t.Priority.String(), contract.HdrSchema: "dxps.v1.TaskResult",
		contract.HdrProducer: "adapter-" + t.Domain,
	})}
}

func (a *Adapter) Run(ctx context.Context, cfg ServiceConfig) error {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if len(cfg.Domains) == 0 {
		cfg.Domains = contract.Domains
	}
	var wg sync.WaitGroup
	for _, d := range cfg.Domains {
		for p := contract.P0; p <= contract.P3; p++ {
			wg.Add(1)
			go func(d string, p contract.Priority) {
				defer wg.Done()
				err := bus.RunEOS(ctx, bus.EOSConfig{Brokers: cfg.Brokers, Group: "dxps-adapter-" + d + "-" + p.String(),
					TransactionID: "dxps-adapter-" + d + "-" + p.String() + "-" + cfg.Instance,
					Topics:        []string{contract.TaskTopic(d, p)}, Concurrency: tierConcurrency[p], Process: a.Process, Log: cfg.Log})
				if err != nil && ctx.Err() == nil {
					cfg.Log.Error("eos loop stopped", "domain", d, "tier", p, "err", err)
				}
			}(d, p)
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.health(ctx, cfg)
	}()
	if cfg.StatsAddr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.serveStats(ctx, cfg.StatsAddr); err != nil {
				cfg.Log.Error("stats", "err", err)
			}
		}()
	}
	wg.Wait()
	return nil
}

// health refreshes the registry and publishes NE breaker state to the compacted dxps.state.ne-health topic.
func (a *Adapter) health(ctx context.Context, cfg ServiceConfig) {
	p, err := bus.NewProducer(cfg.Brokers, "adapter-health")
	if err != nil {
		cfg.Log.Error("health producer", "err", err)
		return
	}
	defer p.Close()
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if cfg.Refresh != nil {
				if err := cfg.Refresh(ctx); err != nil {
					cfg.Log.Warn("registry refresh", "err", err)
				}
			}
			for _, s := range a.Stats() {
				_ = p.PublishJSON(ctx, contract.TopicStateNE, s.NE, "host-mno", "p2", s)
			}
		}
	}
}

func (a *Adapter) serveStats(ctx context.Context, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || (host != "127.0.0.1" && host != "localhost") {
		return errors.New("stats endpoint must bind to loopback")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(a.Stats())
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
