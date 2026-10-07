package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"dxps/internal/auth"
	"dxps/internal/ids"
	"dxps/internal/pki"
	"dxps/internal/store"
)

const basePath = "/tmf-api/serviceOrdering/v5"

// Demo drives realistic TMF641 traffic through the gateway and runs live security probes.
type Demo struct {
	s *Server

	mu       sync.Mutex
	running  bool
	started  time.Time
	target   int
	sent     int64
	byStatus map[int]int64
	byCode   map[string]int64
	bySpec   map[string]int64
	lats     []float64
	lastErr  string
	hubs     map[string]bool
	created  map[string][]subscriber
	counter  atomic.Int64
	Probes   []ProbeResult
	probing  bool
}

type subscriber struct{ supi, msisdn string }

type DemoStatus struct {
	Running  bool             `json:"running"`
	Started  time.Time        `json:"started"`
	Target   int              `json:"target"`
	Sent     int64            `json:"sent"`
	ByStatus map[int]int64    `json:"byStatus"`
	ByCode   map[string]int64 `json:"byCode"`
	BySpec   map[string]int64 `json:"bySpec"`
	P50ms    float64          `json:"p50ms"`
	P99ms    float64          `json:"p99ms"`
	LastErr  string           `json:"lastError,omitempty"`
	Probing  bool             `json:"probing"`
}

func (d *Demo) Status() DemoStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := DemoStatus{Running: d.running, Started: d.started, Target: d.target, Sent: d.sent, ByStatus: map[int]int64{},
		ByCode: map[string]int64{}, BySpec: map[string]int64{}, LastErr: d.lastErr, Probing: d.probing}
	for k, v := range d.byStatus {
		st.ByStatus[k] = v
	}
	for k, v := range d.byCode {
		st.ByCode[k] = v
	}
	for k, v := range d.bySpec {
		st.BySpec[k] = v
	}
	l := append([]float64(nil), d.lats...)
	sort.Float64s(l)
	if n := len(l); n > 0 {
		st.P50ms, st.P99ms = l[n/2], l[min(n-1, n*99/100)]
	}
	return st
}

func (d *Demo) ProbeResults() []ProbeResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]ProbeResult(nil), d.Probes...)
}

type demoReq struct {
	Count int     `json:"count"`
	Rate  int     `json:"rate"`
	Chaos float64 `json:"chaos"`
}

func (d *Demo) start(w http.ResponseWriter, r *http.Request) {
	var q demoReq
	if err := decode(r, &q); err != nil || q.Count < 1 || q.Count > 20000 || q.Rate < 1 || q.Rate > 500 || q.Chaos < 0 || q.Chaos > 1 {
		writeJSON(w, 400, map[string]string{"error": "count 1..20000, rate 1..500/s, chaos 0..1"})
		return
	}
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		writeJSON(w, 409, map[string]string{"error": "demo already running"})
		return
	}
	d.running, d.started, d.target, d.sent = true, time.Now(), q.Count, 0
	d.byStatus, d.byCode, d.bySpec, d.lats, d.lastErr = map[int]int64{}, map[string]int64{}, map[string]int64{}, nil, ""
	if d.hubs == nil {
		d.hubs, d.created = map[string]bool{}, map[string][]subscriber{}
		d.counter.Store((time.Now().Unix() % 900) * 10000)
	}
	d.mu.Unlock()
	go d.run(q)
	writeJSON(w, 202, map[string]any{"accepted": true, "count": q.Count, "rate": q.Rate})
}

var demoTenants = []string{"host-mno", "mvno-alpha", "mvno-beta", "ent-acme"}

func tenantDigit(t string) int {
	for i, x := range demoTenants {
		if x == t {
			return i + 1
		}
	}
	return 9
}

func (d *Demo) token(t string, scopes ...string) string {
	if len(scopes) == 0 {
		scopes = []string{auth.ScopeOrderWrite, auth.ScopeOrderRead, auth.ScopeHubWrite}
	}
	tok, _ := d.s.Signer.Issue("dashboard-demo", t, scopes, 10*time.Minute)
	return tok
}

func (d *Demo) call(ctx context.Context, method, path, tok string, body []byte, hdr map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, d.s.GWURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := d.s.Gateway.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, nil
}

// ensureHubs registers one TMF688 listener per tenant (pointing at the netsim webhook sink).
func (d *Demo) ensureHubs(ctx context.Context) {
	for _, t := range demoTenants {
		d.mu.Lock()
		done := d.hubs[t]
		d.mu.Unlock()
		if done {
			continue
		}
		cb := "https://localhost:9109/hooks/" + t
		exists := false
		_ = d.s.Store.InTenant(ctx, t, func(tx pgx.Tx) error {
			hs, err := store.ListHubs(ctx, tx)
			for _, h := range hs {
				exists = exists || h.Callback == cb
			}
			return err
		})
		if !exists {
			b, _ := json.Marshal(map[string]string{"callback": cb})
			if st, _, err := d.call(ctx, http.MethodPost, basePath+"/hub", d.token(t), b, nil); err != nil || st != 201 {
				continue
			}
		}
		d.mu.Lock()
		d.hubs[t] = true
		d.mu.Unlock()
	}
}

func (d *Demo) run(q demoReq) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(q.Count/q.Rate+120)*time.Second)
	defer cancel()
	defer func() { d.mu.Lock(); d.running = false; d.mu.Unlock() }()
	d.ensureHubs(ctx)
	toks := map[string]string{}
	for _, t := range demoTenants {
		toks[t] = d.token(t)
	}
	work := make(chan int, 64)
	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range work {
				t := demoTenants[weightedTenant()]
				spec, body := d.order(t, q.Chaos)
				start := time.Now()
				st, resp, err := d.call(ctx, http.MethodPost, basePath+"/serviceOrder", toks[t], body,
					map[string]string{"Idempotency-Key": "demo-" + ids.NewString()})
				el := float64(time.Since(start).Microseconds()) / 1000
				var e struct{ Code string }
				if st >= 400 {
					_ = json.Unmarshal(resp, &e)
				}
				d.mu.Lock()
				d.sent++
				d.byStatus[st]++
				d.bySpec[spec]++
				if e.Code != "" {
					d.byCode[e.Code]++
				}
				if err != nil {
					d.lastErr = err.Error()
				}
				d.lats = append(d.lats, el)
				if len(d.lats) > 5000 {
					d.lats = d.lats[len(d.lats)-5000:]
				}
				d.mu.Unlock()
			}
		}()
	}
	tick := time.NewTicker(time.Second / time.Duration(q.Rate))
	defer tick.Stop()
	for i := 0; i < q.Count; i++ {
		select {
		case <-ctx.Done():
			i = q.Count
		case <-tick.C:
			work <- i
		}
	}
	close(work)
	wg.Wait()
}

// weightedTenant: the host MNO generates most traffic, MVNOs and the enterprise less.
func weightedTenant() int {
	switch x := rand.IntN(100); {
	case x < 40:
		return 0
	case x < 70:
		return 1
	case x < 88:
		return 2
	default:
		return 3
	}
}

func (d *Demo) next() int64 {
	for {
		c := d.counter.Add(1)
		if c%1000 < 990 {
			return c
		}
	}
}

func chaosSuffix(s string, chaos float64) string {
	if chaos == 0 || rand.Float64() >= chaos {
		return s
	}
	suf := "997" // transient: two 503s then success (retry ladder)
	switch x := rand.IntN(100); {
	case x < 35:
		suf = "998" // permanent rejection (saga compensation)
	case x < 40:
		suf = "999" // NE unavailable (retries until exhausted)
	}
	return s[:len(s)-3] + suf
}

func char(name string, v any) map[string]any {
	b, _ := json.Marshal(v)
	return map[string]any{"name": name, "value": json.RawMessage(b)}
}

func item(id, spec string, params map[string]any, deps ...string) map[string]any {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cs := make([]any, 0, len(keys))
	for _, k := range keys {
		cs = append(cs, char(k, params[k]))
	}
	it := map[string]any{"id": id, "action": "add", "@type": "ServiceOrderItem",
		"service": map[string]any{"@type": "Service", "serviceSpecification": map[string]any{"id": spec}, "serviceCharacteristic": cs}}
	if len(deps) > 0 {
		var rels []any
		for _, dep := range deps {
			rels = append(rels, map[string]any{"relationshipType": "dependsOn", "orderItem": map[string]any{"itemId": dep}})
		}
		it["serviceOrderItemRelationship"] = rels
	}
	return it
}

func (d *Demo) remember(t string, s subscriber) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := append(d.created[t], s)
	if len(l) > 500 {
		l = l[len(l)-500:]
	}
	d.created[t] = l
}

func (d *Demo) existing(t string) (subscriber, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l := d.created[t]
	if len(l) == 0 {
		return subscriber{}, false
	}
	return l[rand.IntN(len(l))], true
}

// order builds a realistic TMF641 ServiceOrder for tenant t; the returned name labels the scenario.
func (d *Demo) order(t string, chaos float64) (string, []byte) {
	c := d.next()
	td := tenantDigit(t)
	sub := subscriber{supi: fmt.Sprintf("imsi-41677%d%09d", td, c), msisdn: fmt.Sprintf("9627%d%07d", td, c%10000000)}
	newSub := func() map[string]any {
		sub.msisdn = chaosSuffix(sub.msisdn, chaos)
		plan := []string{"5G-100GB", "5G-UNLIMITED", "LTE-20GB"}[rand.IntN(3)]
		if t == "mvno-alpha" {
			plan = "5G-ALPHA-50"
		}
		d.remember(t, sub)
		return map[string]any{"supi": sub.supi, "msisdn": sub.msisdn, "plan": plan}
	}
	var name string
	var items []any
	prio := ""
	x := rand.IntN(100)
	switch t {
	case "ent-acme":
		if x < 70 {
			bw := 100 + rand.IntN(19)*100
			if chaos > 0 && rand.Float64() < chaos/2 {
				bw = 20000
			}
			name, items = "CreateL3VPN", []any{item("vpn", "CreateL3VPN", map[string]any{"vpnId": "ACME-CORP",
				"siteId": fmt.Sprintf("SITE-%05d", c%100000), "prefix": fmt.Sprintf("10.%d.%d.0/24", c/250%250, c%250), "bandwidthMbps": bw})}
		} else {
			name, items = "QoDSession", []any{item("qod", "QoDSession", map[string]any{"msisdn": sub.msisdn, "profile": "QOS_L", "durationSec": 3600})}
		}
	default:
		switch {
		case x < 30:
			name, items = "CreateSubscriber5G", []any{item("sub", "CreateSubscriber5G", newSub())}
		case x < 45 && t != "mvno-alpha":
			p := newSub()
			name, items = "CreateSubscriber5G+AddVoNR", []any{item("sub", "CreateSubscriber5G", p),
				item("volte", "AddVoNR", map[string]any{"supi": p["supi"], "msisdn": p["msisdn"]}, "sub")}
		case x < 58:
			eid := fmt.Sprintf("89049032%d%023d", td, c)
			eid = chaosSuffix(eid, chaos)
			name, items = "ProvisionESIM", []any{item("esim", "ProvisionESIM", map[string]any{"eid": eid, "msisdn": sub.msisdn, "profileType": "5G-CONSUMER"})}
		case x < 68:
			if s, ok := d.existing(t); ok {
				name, items = "ChangePlan", []any{item("plan", "ChangePlan", map[string]any{"supi": s.supi, "msisdn": s.msisdn, "plan": "5G-UNLIMITED"})}
			} else {
				name, items = "CreateSubscriber5G", []any{item("sub", "CreateSubscriber5G", newSub())}
			}
		case x < 74:
			if s, ok := d.existing(t); ok {
				name, items, prio = "SuspendSubscriber", []any{item("bar", "SuspendSubscriber", map[string]any{"supi": s.supi, "reason": "FRAUD"})}, "0"
			} else {
				name, items = "CreateSubscriber5G", []any{item("sub", "CreateSubscriber5G", newSub())}
			}
		case x < 82 && t != "mvno-beta":
			name, items = "PortInNumber", []any{item("port", "PortInNumber", map[string]any{"msisdn": fmt.Sprintf("9627%d8%06d", td, c%1000000), "donor": "OPERATOR-X"})}
		case x < 90 && t == "host-mno":
			cpe := fmt.Sprintf("os::00D09E-CPE%07d", c%10000000)
			if chaos > 0 && rand.Float64() < chaos/2 {
				cpe = "os::00D09E-OFFLINE"
			}
			name, items = "ActivateFTTH", []any{item("ftth", "ActivateFTTH", map[string]any{"ontSerial": fmt.Sprintf("HWTC%08X", c),
				"oltPort": fmt.Sprintf("1/1/%d", c%16+1), "vlan": 100 + int(c%3000), "cpeEndpoint": cpe, "speedMbps": 1000})}
		case x < 95:
			name, items = "QoDSession", []any{item("qod", "QoDSession", map[string]any{"msisdn": sub.msisdn, "profile": "QOS_E", "durationSec": 600})}
		default:
			if s, ok := d.existing(t); ok {
				name, items = "DeleteSubscriber5G", []any{item("del", "DeleteSubscriber5G", map[string]any{"supi": s.supi, "msisdn": s.msisdn})}
			} else {
				name, items = "CreateSubscriber5G", []any{item("sub", "CreateSubscriber5G", newSub())}
			}
		}
	}
	o := map[string]any{"@type": "ServiceOrder", "externalId": fmt.Sprintf("DEMO-%s-%d", strings.ToUpper(t), c), "category": "demo",
		"description": name, "channel": []any{map[string]any{"name": "DASHBOARD", "role": "orderChannel"}}, "serviceOrderItem": items}
	if prio != "" {
		o["priority"] = prio
	}
	b, _ := json.Marshal(o)
	return name, b
}

// ---------------------------------------------------------------- live security probes

type ProbeResult struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Name     string `json:"name"`
	Expect   string `json:"expect"`
	Got      string `json:"got"`
	Pass     bool   `json:"pass"`
}

func (d *Demo) probe(w http.ResponseWriter, r *http.Request) {
	var q struct{}
	if err := decode(r, &q); err != nil && err != io.EOF {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	d.mu.Lock()
	if d.probing {
		d.mu.Unlock()
		writeJSON(w, 409, map[string]string{"error": "probes already running"})
		return
	}
	d.probing = true
	d.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res := d.runProbes(ctx)
		d.mu.Lock()
		d.Probes, d.probing = res, false
		d.mu.Unlock()
	}()
	writeJSON(w, 202, map[string]bool{"started": true})
}

func b64(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }

func validOrder(t string) []byte {
	b, _ := json.Marshal(map[string]any{"serviceOrderItem": []any{item("q", "QoDSession",
		map[string]any{"msisdn": fmt.Sprintf("9627%d5%06d", tenantDigit(t), rand.IntN(1000000)), "profile": "QOS_S", "durationSec": 60})}})
	return b
}

func (d *Demo) runProbes(ctx context.Context) []ProbeResult {
	var out []ProbeResult
	add := func(id, cat, name, expect string, got string, pass bool) {
		out = append(out, ProbeResult{id, cat, name, expect, got, pass})
	}
	status := func(want int, method, path, tok string, body []byte, hdr map[string]string) (string, bool) {
		st, b, err := d.call(ctx, method, path, tok, body, hdr)
		if err != nil {
			return "error: " + err.Error(), false
		}
		var e struct{ Code string }
		_ = json.Unmarshal(b, &e)
		got := fmt.Sprintf("%d %s", st, e.Code)
		return strings.TrimSpace(got), st == want
	}
	idem := func() map[string]string { return map[string]string{"Idempotency-Key": "probe-" + ids.NewString()} }
	ord := basePath + "/serviceOrder"
	good := d.token("mvno-beta")
	parts := strings.Split(good, ".")

	g, p := status(401, http.MethodGet, ord, "", nil, nil)
	add("SEC-01", "AuthN", "No bearer token", "401", g, p)
	none := b64(map[string]string{"alg": "none", "typ": "JWT"}) + "." + parts[1] + "."
	g, p = status(401, http.MethodGet, ord, none, nil, nil)
	add("SEC-02", "AuthN", "JWT alg=none", "401", g, p)
	sig := []byte(parts[2])
	sig[3] ^= 1
	g, p = status(401, http.MethodGet, ord, parts[0]+"."+parts[1]+"."+string(sig), nil, nil)
	add("SEC-03", "AuthN", "Tampered JWT signature", "401", g, p)
	var claims map[string]any
	pb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(pb, &claims)
	claims["tenant"] = "host-mno"
	g, p = status(401, http.MethodGet, ord, parts[0]+"."+b64(claims)+"."+parts[2], nil, nil)
	add("SEC-04", "AuthN", "Tenant claim swapped (signature reused)", "401", g, p)
	old, _ := auth.NewSigner(d.s.JWTKey, d.s.Issuer, d.s.Audience)
	old.SetClock(func() time.Time { return time.Now().Add(-2 * time.Hour) })
	exp, _ := old.Issue("probe", "mvno-beta", []string{auth.ScopeOrderRead}, time.Minute)
	g, p = status(401, http.MethodGet, ord, exp, nil, nil)
	add("SEC-05", "AuthN", "Expired JWT", "401", g, p)
	aud, _ := auth.NewSigner(d.s.JWTKey, d.s.Issuer, "other-api")
	at, _ := aud.Issue("probe", "mvno-beta", []string{auth.ScopeOrderRead}, time.Minute)
	g, p = status(401, http.MethodGet, ord, at, nil, nil)
	add("SEC-06", "AuthN", "Wrong audience", "401", g, p)

	g, p = status(403, http.MethodGet, ord, good, nil, map[string]string{"X-Tenant": "host-mno"})
	add("SEC-07", "AuthZ", "X-Tenant impersonation without dxps:tenant:any", "403 DXPS-1004", g, p)
	ro := d.token("mvno-beta", auth.ScopeOrderRead)
	g, p = status(403, http.MethodPost, ord, ro, validOrder("mvno-beta"), idem())
	add("SEC-08", "AuthZ", "Write with read-only scope", "403", g, p)
	g, p = status(403, http.MethodGet, ord, d.token("mvno-gamma"), nil, nil)
	add("SEC-09", "AuthZ", "Suspended tenant", "403 DXPS-1004", g, p)

	hostTok := d.token("host-mno")
	st, b, err := d.call(ctx, http.MethodPost, ord, hostTok, validOrder("host-mno"), idem())
	var so struct{ ID string }
	_ = json.Unmarshal(b, &so)
	if err == nil && st == 201 && so.ID != "" {
		g, p = status(404, http.MethodGet, ord+"/"+so.ID, good, nil, nil)
		add("SEC-10", "Isolation", "Cross-tenant read of another tenant's order (RLS)", "404", g, p)
		g, p = status(200, http.MethodGet, ord+"/"+so.ID, hostTok, nil, nil)
		add("SEC-11", "Isolation", "Owner can read the same order", "200", g, p)
	} else {
		add("SEC-10", "Isolation", "Cross-tenant read of another tenant's order (RLS)", "404", fmt.Sprintf("setup failed %d", st), false)
	}

	g, p = status(400, http.MethodPost, ord, good, []byte(`{"serviceOrderItem":[],"evil":true}`), idem())
	add("SEC-12", "Input", "Unknown JSON field", "400 DXPS-1002", g, p)
	g, p = status(400, http.MethodPost, ord, good, append(validOrder("mvno-beta"), []byte(`{"x":1}`)...), idem())
	add("SEC-13", "Input", "Trailing JSON (request smuggling)", "400 DXPS-1002", g, p)
	big := bytes.Repeat([]byte("A"), 300<<10)
	g, p = status(413, http.MethodPost, ord, good, append(append([]byte(`{"description":"`), big...), []byte(`"}`)...), idem())
	add("SEC-14", "Input", "Oversized body (300 KiB)", "413 DXPS-1002", g, p)
	inj, _ := json.Marshal(map[string]any{"serviceOrderItem": []any{item("i", "SuspendSubscriber",
		map[string]any{"supi": "imsi-1'; DROP TABLE dxps.service_order;--", "reason": "FRAUD"})}})
	g, p = status(400, http.MethodPost, ord, good, inj, idem())
	add("SEC-15", "Input", "SQL injection in characteristic", "400 DXPS-1002", g, p)
	g, p = status(404, http.MethodGet, ord+"/'%20OR%201=1--", good, nil, nil)
	add("SEC-16", "Input", "SQL injection in path id", "404", g, p)
	g, p = status(400, http.MethodPost, ord, good, validOrder("mvno-beta"), nil)
	add("SEC-17", "Input", "Missing Idempotency-Key", "400", g, p)
	k := idem()
	_, _, _ = d.call(ctx, http.MethodPost, ord, good, validOrder("mvno-beta"), k)
	g, p = status(409, http.MethodPost, ord, good, validOrder("mvno-beta"), k)
	add("SEC-18", "Input", "Idempotency-Key reused with different body", "409 DXPS-1008", g, p)

	hub := basePath + "/hub"
	g, p = status(400, http.MethodPost, hub, good, []byte(`{"callback":"https://169.254.169.254/latest/meta-data"}`), nil)
	add("SEC-19", "SSRF", "Webhook to cloud metadata address", "400", g, p)
	g, p = status(400, http.MethodPost, hub, good, []byte(`{"callback":"http://localhost:9109/hooks"}`), nil)
	add("SEC-20", "SSRF", "Webhook over plain http", "400", g, p)
	g, p = status(400, http.MethodPost, hub, good, []byte(`{"callback":"https://user:pw@localhost:9109/x"}`), nil)
	add("SEC-21", "SSRF", "Webhook with userinfo", "400", g, p)

	acme := d.token("ent-acme", auth.ScopeOrderRead)
	var n429 atomic.Int64
	var wg sync.WaitGroup
	for range 80 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if st, _, _ := d.call(ctx, http.MethodGet, ord+"?limit=1", acme, nil, nil); st == 429 {
				n429.Add(1)
			}
		}()
	}
	wg.Wait()
	add("SEC-22", "Abuse", "Per-tenant rate limit (80 parallel calls)", "some 429 DXPS-1006", fmt.Sprintf("%d x 429", n429.Load()), n429.Load() > 0)

	g, p = d.mtlsProbe(ctx, nil)
	add("SEC-23", "mTLS", "NE simulator without client certificate", "handshake rejected", g, p)
	ca, _ := pki.NewCA("rogue-ca", time.Hour)
	var rogue *tls.Certificate
	if ca != nil {
		if bnd, err := ca.Issue(pki.LeafOpts{CommonName: "adapter", Client: true, Validity: time.Hour}); err == nil {
			if c, err := tls.X509KeyPair(bnd.CertPEM, bnd.KeyPEM); err == nil {
				rogue = &c
			}
		}
	}
	g, p = d.mtlsProbe(ctx, rogue)
	add("SEC-24", "mTLS", "NE simulator with certificate from a foreign CA", "handshake rejected", g, p)
	return out
}

func (d *Demo) mtlsProbe(ctx context.Context, cert *tls.Certificate) (string, bool) {
	tr := d.s.Gateway.Transport.(*http.Transport).Clone()
	cfg := tr.TLSClientConfig.Clone()
	cfg.Certificates = nil
	if cert != nil {
		// Present the certificate even though the server's acceptable-CA list does not include its issuer.
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cert, nil }
	}
	target := d.s.Src.NEProbe
	if target == "" {
		target = "https://localhost:9102/nudr-dr/v2/subscription-data/imsi-416770000000001/authentication-data/authentication-subscription"
	}
	u, err := url.Parse(target)
	if err != nil {
		return "bad target", false
	}
	cfg.ServerName, cfg.NextProtos = u.Hostname(), []string{"http/1.1"}
	// An unreachable simulator must not pass: only a refusal after the TCP connect counts.
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return "unreachable: " + shortErr(err), false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	tc := tls.Client(conn, cfg)
	var verr *tls.CertificateVerificationError
	var nerr net.Error
	verdict := func(err error) (string, bool) {
		switch {
		case errors.As(err, &verr):
			return "probe cannot verify the simulator: " + shortErr(err), false
		case errors.As(err, &nerr) && nerr.Timeout():
			return "no verdict (timeout)", false
		}
		return "rejected: " + shortErr(err), true
	}
	if err := tc.HandshakeContext(ctx); err != nil {
		return verdict(err)
	}
	// TLS 1.3 verifies the client certificate after the client's Finished, so the server's verdict (alert or
	// reset, depending on the OS) arrives with the first read.
	if _, err := fmt.Fprintf(tc, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", u.RequestURI(), u.Host); err != nil {
		return verdict(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		return verdict(err)
	}
	resp.Body.Close()
	return fmt.Sprintf("accepted %d", resp.StatusCode), false
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return truncate(s, 60)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// GatewayClient returns an HTTPS client that trusts only the DxPS dev CA.
func GatewayClient(caPEM []byte) (*http.Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("invalid CA")
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 64,
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}}}, nil
}
