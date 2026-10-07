// netsim runs the network element simulators on loopback: NE ports require mutual TLS; the
// webhook sink uses server TLS only (it plays an MVNO listener).
package main

import (
	"flag"

	"dxps/internal/netsim"
	"dxps/internal/pki"
	"dxps/internal/svc"
)

func main() {
	admin := flag.String("admin", "127.0.0.1:9199", "loopback admin address")
	flag.Parse()
	ctx, cancel, cfg, log := svc.Boot("netsim")
	defer cancel()
	neTLS, err := pki.ServerTLSFiles(cfg.PKIDir, "netsim", true)
	if err != nil {
		svc.Fatal(log, "tls", err)
	}
	sinkTLS, err := pki.ServerTLSFiles(cfg.PKIDir, "netsim", false)
	if err != nil {
		svc.Fatal(log, "tls", err)
	}
	n := netsim.New(cfg.NetsimHost, netsim.AdminToken(cfg.JWTKey))
	if err := n.Serve(neTLS, sinkTLS); err != nil {
		svc.Fatal(log, "serve", err)
	}
	defer n.Close()
	srv, err := netsim.ServeAdmin(*admin, n.AdminHandler())
	if err != nil {
		svc.Fatal(log, "admin", err)
	}
	defer srv.Close()
	log.Info("simulators up", "ports", n.Ports())
	<-ctx.Done()
}
