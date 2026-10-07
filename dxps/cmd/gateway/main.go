// gateway serves the TMF641 Service Ordering and TMF688 hub APIs over HTTPS (TLS 1.3, HTTP/2).
package main

import (
	"context"
	"flag"
	"time"

	"dxps/internal/auth"
	"dxps/internal/bus"
	"dxps/internal/catalog"
	"dxps/internal/gateway"
	"dxps/internal/pki"
	"dxps/internal/secretbox"
	"dxps/internal/store"
	"dxps/internal/svc"
	"dxps/internal/tenant"
)

type publisher struct {
	p *bus.Producer
	s *store.Store
}

func (b publisher) Publish(ctx context.Context, rows []*store.OutboxRow) error {
	return bus.FastPublish(ctx, b.p, b.s, rows)
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8443", "listen address")
	flag.Parse()
	ctx, cancel, cfg, log := svc.Boot("gateway")
	defer cancel()

	st := svc.OpenStore(ctx, cfg, log)
	defer st.Close()
	cat, err := catalog.Load()
	if err != nil {
		svc.Fatal(log, "catalog", err)
	}
	signer, err := auth.NewSigner(cfg.JWTKey, cfg.Issuer, cfg.Audience)
	if err != nil {
		svc.Fatal(log, "signer", err)
	}
	box, err := secretbox.New(cfg.MasterKey)
	if err != nil {
		svc.Fatal(log, "secretbox", err)
	}
	p, err := bus.NewProducer(cfg.Kafka, "gateway")
	if err != nil {
		svc.Fatal(log, "producer", err)
	}
	defer p.Close()
	tr := tenant.NewRegistry()
	refresh := func() {
		ts, err := st.LoadTenants(ctx)
		if err != nil {
			log.Warn("tenant refresh", "err", err)
			return
		}
		tr.Replace(ts)
	}
	refresh()
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			}
		}
	}()
	tlsCfg, err := pki.ServerTLSFiles(cfg.PKIDir, "gateway", false)
	if err != nil {
		svc.Fatal(log, "tls", err)
	}
	g := &gateway.Gateway{Store: st, Catalog: cat, Tenants: tr, Signer: signer, Pub: publisher{p, st}, Box: box,
		HubAllow: cfg.HubAllow, Log: log}
	srv := svc.Server(*addr, g.Handler())
	srv.TLSConfig = tlsCfg
	go svc.Shutdown(ctx, srv)
	log.Info("listening", "addr", *addr)
	if err := srv.ListenAndServeTLS("", ""); !svc.Ignore(err) {
		svc.Fatal(log, "serve", err)
	}
}
