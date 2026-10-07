// dashboard serves the DxPS Mosaic on loopback.
package main

import (
	"flag"
	"net"
	"path/filepath"

	"dxps/internal/auth"
	"dxps/internal/bus"
	"dxps/internal/catalog"
	"dxps/internal/dashboard"
	"dxps/internal/netsim"
	"dxps/internal/pki"
	"dxps/internal/svc"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8088", "loopback listen address")
	flag.Parse()
	ctx, cancel, cfg, log := svc.Boot("dashboard")
	defer cancel()
	st := svc.OpenStore(ctx, cfg, log)
	defer st.Close()
	adm, err := bus.NewAdmin(cfg.Kafka)
	if err != nil {
		svc.Fatal(log, "kafka admin", err)
	}
	defer adm.Close()
	cat, err := catalog.Load()
	if err != nil {
		svc.Fatal(log, "catalog", err)
	}
	signer, err := auth.NewSigner(cfg.JWTKey, cfg.Issuer, cfg.Audience)
	if err != nil {
		svc.Fatal(log, "signer", err)
	}
	caPEM, err := pki.ReadCAPEM(cfg.PKIDir)
	if err != nil {
		svc.Fatal(log, "ca", err)
	}
	gw, err := dashboard.GatewayClient(caPEM)
	if err != nil {
		svc.Fatal(log, "gateway client", err)
	}
	ln, err := dashboard.ListenLoopback(*addr)
	if err != nil {
		svc.Fatal(log, "listen", err)
	}
	_, port, _ := net.SplitHostPort(*addr)
	d := dashboard.New(&dashboard.Server{Store: st, Admin: adm, Catalog: cat, Signer: signer, JWTKey: cfg.JWTKey,
		Issuer: cfg.Issuer, Audience: cfg.Audience, Gateway: gw, GWURL: cfg.GatewayURL, Log: log,
		Origins: []string{"http://127.0.0.1:" + port, "http://localhost:" + port},
		Src: dashboard.Sources{Orchestrator: "http://127.0.0.1:9201/stats", Adapter: "http://127.0.0.1:9202/stats",
			EventHub: "http://127.0.0.1:9203/stats", NetsimAdmin: "http://127.0.0.1:9199",
			NetsimToken: netsim.AdminToken(cfg.JWTKey), Results: filepath.Join(cfg.Runtime, "results", "tests.json")}})
	srv := svc.Server("", d.Handler())
	go svc.Shutdown(ctx, srv)
	log.Info("mosaic ready", "url", "http://"+*addr)
	if err := srv.Serve(ln); !svc.Ignore(err) {
		svc.Fatal(log, "serve", err)
	}
}
