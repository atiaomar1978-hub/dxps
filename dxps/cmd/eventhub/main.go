// eventhub delivers TMF688 order state events to tenant listeners with HMAC-signed webhooks.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"dxps/internal/bus"
	"dxps/internal/contract"
	"dxps/internal/eventhub"
	"dxps/internal/pki"
	"dxps/internal/secretbox"
	"dxps/internal/svc"
	"dxps/internal/tenant"
)

func main() {
	instance := flag.String("instance", "", "static group member id (empty = dynamic)")
	stats := flag.String("stats", "127.0.0.1:9203", "loopback stats address")
	flag.Parse()
	ctx, cancel, cfg, log := svc.Boot("eventhub")
	defer cancel()
	st := svc.OpenStore(ctx, cfg, log)
	defer st.Close()

	box, err := secretbox.New(cfg.MasterKey)
	if err != nil {
		svc.Fatal(log, "secretbox", err)
	}
	tlsCfg, err := pki.ClientTLSFiles(cfg.PKIDir, "eventhub")
	if err != nil {
		svc.Fatal(log, "tls", err)
	}
	h := &eventhub.Hub{Store: st, Box: box, Allow: cfg.HubAllow, Log: log, Attempts: 4,
		Client: eventhub.NewClient(&http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true}})}
	tr := tenant.NewRegistry()
	if ts, err := st.LoadTenants(ctx); err == nil {
		tr.Replace(ts)
	}
	var tiers [4][]string
	tiers[contract.P1] = []string{contract.TopicEvent}
	c, err := bus.NewTieredConsumer(bus.ConsumerConfig{Brokers: cfg.Kafka, Group: "dxps-eventhub", Instance: *instance,
		TierTopics: tiers, Concurrency: 64, Quantum: tr.Quantum, Log: log,
		OnError: func(r *kgo.Record, err error) { log.Error("event dropped", "offset", r.Offset, "err", err) },
		Handle: func(ctx context.Context, r *kgo.Record) error {
			ev, err := eventhub.Decode(r.Value, bus.Header(r, contract.HdrTenant))
			if err != nil {
				return err
			}
			return h.Handle(ctx, ev, ev.Tenant, string(r.Key))
		}})
	if err != nil {
		svc.Fatal(log, "consumer", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h.Stats())
	})
	srv := &http.Server{Addr: *stats, Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go svc.Shutdown(ctx, srv)
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Error("stats", "err", err)
		}
	}()
	log.Info("running")
	if err := c.Run(ctx); err != nil && ctx.Err() == nil {
		svc.Fatal(log, "run", err)
	}
}
