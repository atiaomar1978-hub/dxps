// console serves the DxPS operations and configuration console API on loopback (HTTPS, dev CA).
//
//	console [-addr 127.0.0.1:8446] [-catalog DIR] [-data DIR]
//	console useradd -name NAME -roles admin            (password from DXPS_CONSOLE_PASSWORD or stdin)
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dxps/internal/auth"
	"dxps/internal/config"
	"dxps/internal/console"
	"dxps/internal/dashboard"
	"dxps/internal/netsim"
	"dxps/internal/pki"
	"dxps/internal/svc"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "useradd" {
		if err := useradd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "useradd:", err)
			os.Exit(1)
		}
		return
	}
	addr := flag.String("addr", "127.0.0.1:8446", "loopback listen address")
	catDir := flag.String("catalog", os.Getenv("DXPS_CATALOG_DIR"), "catalog source tree (dxps/internal/catalog/data)")
	dataDir := flag.String("data", "", "console state directory (default <runtime>/console)")
	flag.Parse()

	ctx, cancel, cfg, log := svc.Boot("console")
	defer cancel()
	if *dataDir == "" {
		*dataDir = filepath.Join(cfg.Runtime, "console")
	}
	if *catDir == "" {
		svc.Fatal(log, "catalog", errors.New("-catalog or DXPS_CATALOG_DIR is required"))
	}
	if _, err := os.Stat(filepath.Join(*catDir, "specs")); err != nil {
		svc.Fatal(log, "catalog", err)
	}
	host, port, err := net.SplitHostPort(*addr)
	if err != nil {
		svc.Fatal(log, "addr", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		svc.Fatal(log, "addr", errors.New("the console API must bind to loopback"))
	}
	if err := ensureCert(cfg.PKIDir); err != nil {
		svc.Fatal(log, "certificate", err)
	}
	tlsCfg, err := pki.ServerTLSFiles(cfg.PKIDir, "console", false)
	if err != nil {
		svc.Fatal(log, "tls", err)
	}
	st := svc.OpenStore(ctx, cfg, log)
	defer st.Close()
	users, err := console.OpenUsers(filepath.Join(*dataDir, "users.json"), 8*time.Hour)
	if err != nil {
		svc.Fatal(log, "users", err)
	}
	if users.Count() == 0 {
		log.Warn("no console users yet: create one with 'console useradd -name admin -roles admin'")
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
	results := filepath.Join(cfg.Runtime, "results", "tests.json")
	live, err := console.NewLive(console.Endpoints{
		Stats: map[string]string{"orchestrator": "http://127.0.0.1:9201/stats", "adapter": "http://127.0.0.1:9202/stats",
			"eventhub": "http://127.0.0.1:9203/stats"},
		NetsimAdmin: "http://127.0.0.1:9199", NetsimToken: netsim.AdminToken(cfg.JWTKey),
		Mosaic: "http://127.0.0.1:8088", GatewayURL: cfg.GatewayURL, Results: results, Kafka: cfg.Kafka,
	}, gw, signer)
	if err != nil {
		svc.Fatal(log, "kafka admin", err)
	}
	srv := &console.Server{
		Data: &console.PG{St: st}, Live: live, Users: users, Log: log,
		Audit:  console.NewAuditLog(filepath.Join(*dataDir, "audit.jsonl")),
		Drafts: console.NewDrafts(filepath.Join(*dataDir, "drafts")),
		Source: &console.Source{FS: os.DirFS(*catDir)},
		Info: map[string]any{"api": "https://localhost:" + port, "catalogDir": *catDir, "stateDir": *dataDir,
			"runtime": cfg.Runtime, "gateway": cfg.GatewayURL, "kafka": cfg.Kafka, "mosaic": "http://127.0.0.1:8088",
			"results": results, "sessionHours": 8},
	}
	hs := svc.Server(*addr, srv.Handler())
	hs.TLSConfig = tlsCfg
	go svc.Shutdown(ctx, hs)
	log.Info("console api ready", "url", "https://localhost:"+port)
	if err := hs.ListenAndServeTLS("", ""); !svc.Ignore(err) {
		svc.Fatal(log, "serve", err)
	}
}

// ensureCert issues the console server certificate from the DxPS dev CA when it is missing.
func ensureCert(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "console.crt")); err == nil {
		return nil
	}
	c, err := os.ReadFile(filepath.Join(dir, "ca.crt")) // #nosec G304 -- runtime PKI dir
	if err != nil {
		return err
	}
	k, err := os.ReadFile(filepath.Join(dir, "ca.key")) // #nosec G304 -- runtime PKI dir
	if err != nil {
		return err
	}
	ca, err := pki.LoadCA(c, k)
	if err != nil {
		return err
	}
	b, err := ca.Issue(pki.LeafOpts{CommonName: "console", DNS: []string{"localhost"},
		IPs: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, Server: true})
	if err != nil {
		return err
	}
	return pki.WriteBundle(dir, "console", b)
}

func useradd(args []string) error {
	fs := flag.NewFlagSet("useradd", flag.ExitOnError)
	name := fs.String("name", "", "user name")
	roles := fs.String("roles", "viewer", "comma separated roles: viewer, operator, configurator, admin")
	dataDir := fs.String("data", "", "console state directory (default <runtime>/console)")
	fs.Parse(args)
	if *dataDir == "" {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		*dataDir = filepath.Join(cfg.Runtime, "console")
	}
	pw := os.Getenv("DXPS_CONSOLE_PASSWORD")
	if pw == "" {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return errors.New("password expected on stdin or in DXPS_CONSOLE_PASSWORD")
		}
		pw = strings.TrimRight(line, "\r\n")
	}
	u, err := console.OpenUsers(filepath.Join(*dataDir, "users.json"), time.Hour)
	if err != nil {
		return err
	}
	if err := u.Add(*name, pw, strings.Split(*roles, ",")); err != nil {
		return err
	}
	fmt.Println("useradd: created", *name)
	return nil
}
