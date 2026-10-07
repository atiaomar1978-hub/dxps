package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"dxps/internal/auth"
	"dxps/internal/bus"
	"dxps/internal/catalog"
	"dxps/internal/config"
	"dxps/internal/contract"
	"dxps/internal/dashboard"
	"dxps/internal/pki"
	"dxps/internal/seed"
	"dxps/internal/store"
)

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}

func cmdMigrate([]string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	applied, err := store.Migrate(c, cfg.PGOwner)
	if err != nil {
		return err
	}
	fmt.Println("migrate: applied", applied)
	return nil
}

func cmdSeed([]string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	st, err := store.Open(c, "", cfg.PGOps)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, t := range seed.Tenants() {
		if err := st.UpsertTenant(c, t); err != nil {
			return fmt.Errorf("tenant %s: %w", t.ID, err)
		}
	}
	for _, n := range seed.NEs(cfg.NetsimHost) {
		ne := n.NE
		id, err := st.UpsertNE(c, &ne, "vault://dxps/"+ne.Tenant+"/ne/"+ne.Code)
		if err != nil {
			return fmt.Errorf("ne %s: %w", ne.Code, err)
		}
		for _, a := range n.Access {
			if err := st.UpsertAccess(c, ne.Tenant, id, a); err != nil {
				return fmt.Errorf("access %s/%s: %w", ne.Code, a.Tenant, err)
			}
		}
	}
	cat, err := catalog.Load()
	if err != nil {
		return err
	}
	for _, sp := range cat.All() {
		if err := st.UpsertSpec(c, sp); err != nil {
			return fmt.Errorf("spec %s: %w", sp.Code, err)
		}
	}
	p, err := bus.NewProducer(cfg.Kafka, "dxpsctl")
	if err == nil {
		defer p.Close()
		for _, t := range seed.Tenants() {
			if err := p.PublishJSON(c, contract.TopicStateTenant, t.ID, t.ID, "p1", t); err != nil {
				fmt.Println("seed: state.tenant publish skipped:", err)
				break
			}
		}
	}
	fmt.Printf("seed: %d tenants, %d NEs, %d specs\n", len(seed.Tenants()), len(seed.NEs(cfg.NetsimHost)), len(cat.All()))
	return nil
}

func cmdTopics([]string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	created, err := bus.EnsureTopics(c, cfg.Kafka)
	if err != nil {
		return err
	}
	fmt.Printf("topics: %d created, %d total\n", len(created), len(bus.Topics()))
	return nil
}

// cmdRedrive replays dead-lettered records to their source topic (business command / task topics only).
func cmdRedrive(args []string) error {
	fs := flag.NewFlagSet("redrive", flag.ExitOnError)
	idle := fs.Duration("idle", 5*time.Second, "stop after no record for this long")
	fs.Parse(args)
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	allowed := map[string]bool{contract.TopicTaskResult: true}
	for _, t := range bus.Topics() {
		if strings.HasPrefix(t.Name, "dxps.bc.") || strings.HasPrefix(t.Name, "dxps.task.") {
			allowed[t.Name] = true
		}
	}
	cl, err := kgo.NewClient(kgo.SeedBrokers(cfg.Kafka...), kgo.ConsumerGroup("dxps-dlq-redrive"), kgo.ConsumeTopics(contract.TopicDLQ),
		kgo.DisableAutoCommit(), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		return err
	}
	defer cl.Close()
	p, err := bus.NewProducer(cfg.Kafka, "dxpsctl-redrive")
	if err != nil {
		return err
	}
	defer p.Close()
	n, skipped := 0, 0
	for {
		c, cancel := context.WithTimeout(context.Background(), *idle)
		fetches := cl.PollFetches(c)
		cancel()
		if fetches.Empty() {
			break
		}
		var out []*kgo.Record
		fetches.EachRecord(func(r *kgo.Record) {
			src := bus.Header(r, "source_topic")
			if !allowed[src] {
				skipped++
				return
			}
			hs := map[string]string{}
			for _, h := range r.Headers {
				if h.Key != "error" && h.Key != "source_topic" {
					hs[h.Key] = string(h.Value)
				}
			}
			out = append(out, bus.Record(src, string(r.Key), r.Value, hs))
		})
		c2, cancel2 := ctx()
		err := p.Publish(c2, out...)
		if err == nil {
			err = cl.CommitUncommittedOffsets(c2)
		}
		cancel2()
		if err != nil {
			return err
		}
		n += len(out)
	}
	fmt.Printf("redrive: %d records replayed, %d skipped\n", n, skipped)
	return nil
}

// cmdAPI calls the gateway as a tenant with a freshly minted token (never printed) and prints the response.
// usage: dxpsctl api -tenant mvno-beta GET /serviceOrder?state=failed   (body from -body or stdin "-")
func cmdAPI(args []string) error {
	fs := flag.NewFlagSet("api", flag.ExitOnError)
	ten := fs.String("tenant", "host-mno", "tenant claim")
	scope := fs.String("scope", "dxps:order:write dxps:order:read dxps:hub:write", "space separated scopes")
	body := fs.String("body", "", "request body (JSON)")
	idem := fs.String("idem", "", "Idempotency-Key header")
	fs.Parse(args)
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: dxpsctl api [-tenant t] METHOD /path")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	s, err := auth.NewSigner(cfg.JWTKey, cfg.Issuer, cfg.Audience)
	if err != nil {
		return err
	}
	tok, err := s.Issue("dxpsctl", *ten, strings.Fields(*scope), 2*time.Minute)
	if err != nil {
		return err
	}
	caPEM, err := pki.ReadCAPEM(cfg.PKIDir)
	if err != nil {
		return err
	}
	cl, err := dashboard.GatewayClient(caPEM)
	if err != nil {
		return err
	}
	var rd io.Reader
	if *body != "" {
		rd = strings.NewReader(*body)
	}
	req, err := http.NewRequest(fs.Arg(0), cfg.GatewayURL+"/tmf-api/serviceOrdering/v5"+fs.Arg(1), rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if *idem != "" {
		req.Header.Set("Idempotency-Key", *idem)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	fmt.Fprintln(os.Stderr, resp.Status)
	fmt.Println(string(b))
	return nil
}

// cmdToken issues a short-lived dev access token (prints only the token, for scripts).
func cmdToken(args []string) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	ten := fs.String("tenant", "host-mno", "tenant claim")
	sub := fs.String("sub", "dev-client", "subject (client id)")
	scope := fs.String("scope", "dxps:order:write dxps:order:read dxps:hub:write", "space separated scopes")
	ttl := fs.Duration("ttl", time.Hour, "lifetime")
	fs.Parse(args)
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	s, err := auth.NewSigner(cfg.JWTKey, cfg.Issuer, cfg.Audience)
	if err != nil {
		return err
	}
	tok, err := s.Issue(*sub, *ten, strings.Fields(*scope), *ttl)
	if err != nil {
		return err
	}
	fmt.Println(tok)
	return nil
}
