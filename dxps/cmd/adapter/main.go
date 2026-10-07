// adapter executes NE tasks for one or more southbound domains over mutual TLS (HTTP/2).
package main

import (
	"context"
	"flag"
	"net/http"
	"strings"
	"time"

	"dxps/internal/adapter"
	"dxps/internal/pki"
	"dxps/internal/registry"
	"dxps/internal/svc"
)

func main() {
	domains := flag.String("domains", "", "comma separated domains (default all)")
	instance := flag.String("instance", "ad-1", "instance id (transactional id suffix)")
	stats := flag.String("stats", "127.0.0.1:9202", "loopback stats address")
	flag.Parse()
	ctx, cancel, cfg, log := svc.Boot("adapter")
	defer cancel()
	st := svc.OpenStore(ctx, cfg, log)
	defer st.Close()

	tlsCfg, err := pki.ClientTLSFiles(cfg.PKIDir, "adapter")
	if err != nil {
		svc.Fatal(log, "tls", err)
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true,
		MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 5 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	reg := registry.New()
	refresh := func(ctx context.Context) error {
		nes, err := st.LoadNEs(ctx)
		if err != nil {
			return err
		}
		reg.Replace(nes)
		return nil
	}
	if err := refresh(ctx); err != nil {
		svc.Fatal(log, "registry", err)
	}
	var ds []string
	if *domains != "" {
		ds = strings.Split(*domains, ",")
	}
	a := adapter.New(reg, client)
	log.Info("running", "domains", ds, "instance", *instance)
	if err := a.Run(ctx, adapter.ServiceConfig{Brokers: cfg.Kafka, Domains: ds, Instance: *instance, StatsAddr: *stats,
		Log: log, Refresh: refresh}); err != nil {
		svc.Fatal(log, "run", err)
	}
}
