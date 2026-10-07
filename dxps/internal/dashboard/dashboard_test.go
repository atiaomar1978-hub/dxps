package dashboard

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dxps/internal/auth"
	"dxps/internal/catalog"
	"dxps/internal/gateway"
	"dxps/internal/netsim"
	"dxps/internal/pki"
	"dxps/internal/secretbox"
	"dxps/internal/seed"
	"dxps/internal/tenant"
	"dxps/internal/testenv"
	"dxps/internal/testenv/pgenv"
)

const origin = "http://127.0.0.1:8088"

var jwtKey = []byte("0123456789abcdef0123456789abcdef")

func newServer(t *testing.T) *Server {
	t.Helper()
	sig, _ := auth.NewSigner(jwtKey, "dxps-test", "dxps-api")
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	return New(&Server{Catalog: cat, Signer: sig, JWTKey: jwtKey, Issuer: "dxps-test", Audience: "dxps-api",
		Origins: []string{origin, "http://localhost:8088"}})
}

func send(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "127.0.0.1:8088"
	for k, v := range hdr {
		if k == "Host" {
			r.Host = v
			continue
		}
		r.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

// session returns valid CSRF headers (cookie + header) for mutating calls.
func session(t *testing.T, h http.Handler) map[string]string {
	t.Helper()
	rr := send(h, "GET", "/api/session", "", nil)
	var m map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	c := rr.Result().Cookies()
	if len(c) != 1 || c[0].Name != "dxps_csrf" || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode || c[0].Value != m["csrf"] {
		t.Fatalf("cookie %+v", c)
	}
	return map[string]string{"Origin": origin, "Cookie": "dxps_csrf=" + m["csrf"], "X-CSRF-Token": m["csrf"], "Content-Type": "application/json"}
}

func with(base map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			delete(out, kv[i])
		} else {
			out[kv[i]] = kv[i+1]
		}
	}
	return out
}

// TC-DSH-001: strict CSP and hardening headers on static assets and API; API responses are not cached.
func TestHeaders(t *testing.T) {
	h := newServer(t).Handler()
	for _, p := range []string{"/", "/app.js", "/api/session"} {
		rr := send(h, "GET", p, "", nil)
		if rr.Code != 200 {
			t.Fatal(p, rr.Code)
		}
		csp := rr.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") ||
			!strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatal(p, csp)
		}
		for _, k := range []string{"X-Content-Type-Options", "Referrer-Policy", "Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy", "Permissions-Policy"} {
			if rr.Header().Get(k) == "" {
				t.Fatal(p, k)
			}
		}
		if strings.HasPrefix(p, "/api/") != (rr.Header().Get("Cache-Control") == "no-store") {
			t.Fatal(p, "cache-control")
		}
	}
}

// TC-DSH-002: DNS-rebinding guard: requests with a foreign Host header are refused (421).
func TestHostGuard(t *testing.T) {
	h := newServer(t).Handler()
	for _, host := range []string{"evil.example:8088", "127.0.0.1:9999", "", "attacker.127.0.0.1.nip.io:8088"} {
		if rr := send(h, "GET", "/api/snapshot", "", map[string]string{"Host": host}); rr.Code != http.StatusMisdirectedRequest {
			t.Fatal(host, rr.Code)
		}
	}
	if rr := send(h, "GET", "/api/session", "", map[string]string{"Host": "LOCALHOST:8088"}); rr.Code != 200 {
		t.Fatal(rr.Code)
	}
}

// TC-DSH-003: mutating endpoints require allow-listed Origin, matching HMAC-bound CSRF cookie+header and JSON.
func TestCSRF(t *testing.T) {
	s := newServer(t)
	h := s.Handler()
	ok := session(t, h)
	forged := strings.Repeat("a", 32) + ".deadbeef"
	other := newServer(t) // token minted with a different key
	otherTok := other.csrfToken(strings.Repeat("b", 32))
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"no origin", with(ok, "Origin", ""), 403},
		{"foreign origin", with(ok, "Origin", "https://evil.example"), 403},
		{"null origin", with(ok, "Origin", "null"), 403},
		{"no cookie", with(ok, "Cookie", ""), 403},
		{"no header", with(ok, "X-CSRF-Token", ""), 403},
		{"mismatch", with(ok, "X-CSRF-Token", ok["X-CSRF-Token"]+"x"), 403},
		{"forged pair", with(ok, "Cookie", "dxps_csrf="+forged, "X-CSRF-Token", forged), 403},
		{"other key", with(ok, "Cookie", "dxps_csrf="+otherTok, "X-CSRF-Token", otherTok), 403},
		{"form post", with(ok, "Content-Type", "application/x-www-form-urlencoded"), 415},
		{"valid", ok, 400}, // reaches the handler; body {} is an invalid demo request
	}
	for _, c := range cases {
		for _, p := range []string{"/api/demo", "/api/fault", "/api/probe"} {
			want := c.want
			if c.name == "valid" && p == "/api/probe" {
				continue
			}
			if rr := send(h, "POST", p, `{"bad":1}`, c.hdr); rr.Code != want {
				t.Fatal(c.name, p, rr.Code, rr.Body)
			}
		}
	}
	if s.csrfValid("nodot") || s.csrfValid("short.abc") {
		t.Fatal("csrfValid")
	}
}

// TC-DSH-004: demo and fault request bounds; 4 KiB body cap on mutating endpoints.
func TestRequestBounds(t *testing.T) {
	h := newServer(t).Handler()
	ok := session(t, h)
	for _, b := range []string{`{"count":0,"rate":1}`, `{"count":20001,"rate":1}`, `{"count":1,"rate":0}`, `{"count":1,"rate":501}`,
		`{"count":1,"rate":1,"chaos":1.5}`, `{"count":1,"rate":1,"chaos":-1}`, `{"count":1,"rate":1,"x":1}`, `{"count":"1"}`} {
		if rr := send(h, "POST", "/api/demo", b, ok); rr.Code != 400 {
			t.Fatal(b, rr.Code)
		}
	}
	for _, b := range []string{`{"sim":"udr","latencyMs":-1}`, `{"sim":"udr","latencyMs":5001}`, `{"sim":"udr","errorRate":2}`, `{"sim":"udr","x":1}`,
		`{"sim":"` + strings.Repeat("a", 5000) + `"}`} {
		if rr := send(h, "POST", "/api/fault", b, ok); rr.Code != 400 {
			t.Fatal(b[:20], rr.Code)
		}
	}
	if rr := send(h, "POST", "/api/probe", `{"x":1}`, ok); rr.Code != 400 {
		t.Fatal(rr.Code)
	}
}

// TC-DSH-005: fault requests are proxied to the netsim admin API with the admin token; unreachable netsim -> 502.
func TestFaultProxy(t *testing.T) {
	ns := netsim.New("localhost", "admin-tok")
	admin := httptest.NewServer(ns.AdminHandler())
	defer admin.Close()
	s := newServer(t)
	s.Src.NetsimAdmin, s.Src.NetsimToken = admin.URL, "admin-tok"
	h := s.Handler()
	ok := session(t, h)
	if rr := send(h, "POST", "/api/fault", `{"sim":"udr","latencyMs":40,"errorRate":0.1}`, ok); rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body)
	}
	for _, st := range ns.Stats() {
		if st.Name == "udr" && (st.Latency != 40 || st.ErrRate != 0.1) {
			t.Fatalf("%+v", st)
		}
	}
	if rr := send(h, "POST", "/api/fault", `{"sim":"nope"}`, ok); rr.Code != 404 {
		t.Fatal(rr.Code)
	}
	s.Src.NetsimToken = "wrong"
	if rr := send(h, "POST", "/api/fault", `{"sim":"udr"}`, ok); rr.Code != 401 {
		t.Fatal(rr.Code)
	}
	admin.Close()
	if rr := send(h, "POST", "/api/fault", `{"sim":"udr"}`, ok); rr.Code != 502 {
		t.Fatal(rr.Code)
	}
}

// TC-DSH-006: the dashboard (which can mint tokens) only binds to loopback.
func TestListenLoopback(t *testing.T) {
	for _, a := range []string{"0.0.0.0:0", ":0", "192.168.1.10:0", "example.com:0", "nope"} {
		if _, err := ListenLoopback(a); err == nil {
			t.Fatal("bound", a)
		}
	}
	for _, a := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
		ln, err := ListenLoopback(a)
		if err != nil {
			if a == "[::1]:0" {
				continue // IPv6 loopback may be disabled
			}
			t.Fatal(a, err)
		}
		ln.Close()
	}
}

// TC-DSH-007: gateway client trusts only the given CA and requires TLS 1.3.
func TestGatewayClient(t *testing.T) {
	if _, err := GatewayClient([]byte("not a pem")); err == nil {
		t.Fatal("invalid CA accepted")
	}
	p := testenv.NewPKI(t)
	c, err := GatewayClient(p.CAPEM)
	if err != nil {
		t.Fatal(err)
	}
	if c.Transport.(*http.Transport).TLSClientConfig.MinVersion != 0x0304 {
		t.Fatal("TLS 1.3 required")
	}
	pub := httptest.NewTLSServer(http.NotFoundHandler()) // certificate from another CA
	defer pub.Close()
	if _, err := c.Get(pub.URL); err == nil {
		t.Fatal("untrusted server accepted")
	}
}

// TC-DSH-008: every generated demo order passes the real gateway validation (catalog, params, CEL, dependsOn).
func TestDemoOrdersValid(t *testing.T) {
	s := newServer(t)
	cat, _ := catalog.Load()
	key := make([]byte, 32)
	rand.Read(key)
	box, _ := secretbox.New(key)
	gw := &gateway.Gateway{Catalog: cat, Tenants: tenant.NewRegistry(seed.Tenants()...), Signer: s.Signer, Box: box}
	h := gw.Handler()
	d := s.demo
	d.hubs, d.created = map[string]bool{}, map[string][]subscriber{}
	seen := map[string]bool{}
	for _, ten := range demoTenants {
		tok := d.token(ten)
		for i := 0; i < 250; i++ {
			chaos := []float64{0, 1}[i%2]
			name, body := d.order(ten, chaos)
			seen[name] = true
			r := httptest.NewRequest("POST", gateway.BasePath+"/serviceOrder", strings.NewReader(string(body)))
			r.Header.Set("Authorization", "Bearer "+tok)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", "demo-test-0000"+string(rune('a'+i%26)))
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			// No store is configured: a request that passes validation reaches storage and is recovered as 500.
			if rr.Code == 400 || rr.Code == 403 {
				t.Fatalf("%s %s: %d %s\n%s", ten, name, rr.Code, rr.Body, body)
			}
			if rr.Code == 429 {
				time.Sleep(20 * time.Millisecond)
			}
		}
	}
	for _, sc := range []string{"CreateSubscriber5G", "CreateSubscriber5G+AddVoNR", "ProvisionESIM", "ChangePlan", "SuspendSubscriber",
		"PortInNumber", "ActivateFTTH", "QoDSession", "DeleteSubscriber5G", "CreateL3VPN"} {
		if !seen[sc] {
			t.Fatal("scenario never generated:", sc)
		}
	}
}

// TC-DSH-009: chaos suffixes, tenant weights, counters and helpers.
func TestDemoHelpers(t *testing.T) {
	if chaosSuffix("1234567890", 0) != "1234567890" {
		t.Fatal("no chaos")
	}
	got := map[string]int{}
	for i := 0; i < 2000; i++ {
		got[chaosSuffix("1234567890", 1)[7:]]++
	}
	if got["997"] < 1000 || got["998"] < 500 || got["999"] < 30 || len(got) != 3 {
		t.Fatal(got)
	}
	w := map[int]int{}
	for i := 0; i < 10000; i++ {
		w[weightedTenant()]++
	}
	if w[0] < w[1] || w[1] < w[2] || w[2] < w[3] || len(w) != 4 {
		t.Fatal(w)
	}
	d := &Demo{}
	d.counter.Store(985)
	for i := 0; i < 20; i++ {
		if c := d.next(); c%1000 >= 990 {
			t.Fatal("fault-like counter", c)
		}
	}
	if tenantDigit("mvno-beta") != 3 || tenantDigit("x") != 9 {
		t.Fatal("tenantDigit")
	}
	if truncate("abcdef", 3) != "abc..." || truncate("ab", 3) != "ab" {
		t.Fatal("truncate")
	}
	if shortErr(io.ErrUnexpectedEOF) != "unexpected EOF" || shortErr(&os.PathError{Op: "open", Path: "x", Err: os.ErrNotExist}) != "file does not exist" {
		t.Fatal(shortErr(io.ErrUnexpectedEOF))
	}
	d.created = map[string][]subscriber{}
	for i := 0; i < 600; i++ {
		d.remember("t", subscriber{supi: "s"})
	}
	if len(d.created["t"]) != 500 {
		t.Fatal(len(d.created["t"]))
	}
	if _, ok := d.existing("none"); ok {
		t.Fatal("existing")
	}
	st := (&Demo{lats: []float64{3, 1, 2}}).Status()
	if st.P50ms != 2 || st.P99ms != 3 {
		t.Fatalf("%+v", st)
	}
}

// ---------------------------------------------------------------- integration

type stack struct {
	s    *Server
	gw   *httptest.Server
	ne   *httptest.Server
	stub atomic.Int64
}

func newStack(t *testing.T) *stack {
	t.Helper()
	st, _ := pgenv.Store(t)
	p := testenv.NewPKI(t)
	s := newServer(t)
	s.Store = st
	key := make([]byte, 32)
	rand.Read(key)
	box, _ := secretbox.New(key)
	gw := &gateway.Gateway{Store: st, Catalog: s.Catalog, Tenants: tenant.NewRegistry(seed.Tenants()...), Signer: s.Signer, Box: box,
		HubAllow: []string{"localhost:9109"}}
	gsrv := httptest.NewUnstartedServer(gw.Handler())
	gsrv.TLS = p.ServerTLS(t, false)
	gsrv.StartTLS()
	t.Cleanup(gsrv.Close)
	ns := netsim.New("localhost", "x")
	ne := httptest.NewUnstartedServer(ns.Handler("udr"))
	ne.TLS = p.ServerTLS(t, true)
	ne.StartTLS()
	t.Cleanup(ne.Close)
	s.Gateway, _ = GatewayClient(p.CAPEM)
	s.GWURL = gsrv.URL
	s.Src.NEProbe = ne.URL + "/nudr-dr/v2/subscription-data/imsi-416770000000001/authentication-data/authentication-subscription"
	return &stack{s: s, gw: gsrv, ne: ne}
}

// TC-DSH-010 (integration): all 24 live security probes pass against a real gateway (PostgreSQL RLS) and an mTLS NE.
func TestSecurityProbesIntegration(t *testing.T) {
	k := newStack(t)
	h := k.s.Handler()
	ok := session(t, h)
	if rr := send(h, "POST", "/api/probe", ``, ok); rr.Code != 202 {
		t.Fatal(rr.Code, rr.Body)
	}
	if rr := send(h, "POST", "/api/probe", `{}`, ok); rr.Code != 409 {
		t.Fatal("concurrent probe run", rr.Code)
	}
	deadline := time.Now().Add(60 * time.Second)
	for k.s.demo.Status().Probing && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	res := k.s.demo.ProbeResults()
	if len(res) != 24 {
		t.Fatal(len(res), res)
	}
	for _, r := range res {
		if !r.Pass {
			t.Errorf("%s %s: expected %s, got %s", r.ID, r.Name, r.Expect, r.Got)
		}
	}
	// With the NE down, mTLS probes must fail instead of reporting a false pass.
	k.ne.Close()
	if g, pass := k.s.demo.mtlsProbe(t.Context(), nil); pass {
		t.Fatal("unreachable NE reported as rejected:", g)
	}
}

// TC-DSH-011 (integration): load generator sends the requested number of orders at the requested rate and
// registers one webhook per tenant; a second start while running is refused (409).
func TestDemoRunIntegration(t *testing.T) {
	k := newStack(t)
	var orders, hubs atomic.Int64
	stub := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/hub"):
			hubs.Add(1)
			w.WriteHeader(201)
		case orders.Add(1)%10 == 0:
			w.WriteHeader(400)
			w.Write([]byte(`{"code":"DXPS-1002"}`))
		default:
			w.WriteHeader(201)
		}
	}))
	stub.TLS = k.gw.TLS
	stub.StartTLS()
	defer stub.Close()
	k.s.GWURL = stub.URL
	h := k.s.Handler()
	ok := session(t, h)
	if rr := send(h, "POST", "/api/demo", `{"count":40,"rate":200,"chaos":0.3}`, ok); rr.Code != 202 {
		t.Fatal(rr.Code, rr.Body)
	}
	if rr := send(h, "POST", "/api/demo", `{"count":1,"rate":1}`, ok); rr.Code != 409 {
		t.Fatal("second start", rr.Code)
	}
	deadline := time.Now().Add(30 * time.Second)
	for k.s.demo.Status().Running && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	st := k.s.demo.Status()
	if st.Sent != 40 || st.ByStatus[201] != 36 || st.ByStatus[400] != 4 || st.ByCode["DXPS-1002"] != 4 || st.P99ms <= 0 {
		t.Fatalf("%+v", st)
	}
	var n int64
	for _, v := range st.BySpec {
		n += v
	}
	if n != 40 {
		t.Fatal(st.BySpec)
	}
	_ = hubs.Load() // tenants that already have the sink registered in the database are skipped
}

// TC-DSH-012 (integration): snapshot aggregates PostgreSQL, loopback stats, netsim and test results; cached ~1 s;
// unavailable sources are reported as down instead of failing the snapshot.
func TestSnapshotIntegration(t *testing.T) {
	k := newStack(t)
	stats := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"ok":true}`)) }))
	defer stats.Close()
	ns := netsim.New("localhost", "tok")
	admin := httptest.NewServer(ns.AdminHandler())
	defer admin.Close()
	dir := t.TempDir()
	res := filepath.Join(dir, "tests.json")
	os.WriteFile(res, []byte(`{"totals":{"tests":1}}`), 0o600)
	k.s.Src = Sources{Orchestrator: stats.URL, Adapter: stats.URL, EventHub: "http://127.0.0.1:1/stats", NetsimAdmin: admin.URL, NetsimToken: "tok", Results: res}
	h := k.s.Handler()
	rr := send(h, "GET", "/api/snapshot", "", nil)
	var snap Snapshot
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if !snap.Up["postgres"] || !snap.Up["orchestrator"] || !snap.Up["adapter"] || snap.Up["eventhub"] || !snap.Up["netsim"] || !snap.Up["gateway"] ||
		snap.Up["kafka"] || snap.DB == nil || len(snap.Tenants) == 0 || len(snap.Catalog) == 0 || string(snap.Tests) != `{"totals":{"tests":1}}` ||
		len(snap.History) != 1 || snap.Errors["eventhub"] == "" {
		t.Fatalf("up=%v errors=%v", snap.Up, snap.Errors)
	}
	rr2 := send(h, "GET", "/api/snapshot", "", nil)
	if rr2.Body.String() != rr.Body.String() {
		t.Fatal("snapshot must be cached for ~1 s")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`not json`)) }))
	defer bad.Close()
	if _, err := k.s.get(t.Context(), bad.URL, ""); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if _, err := k.s.get(t.Context(), "://", ""); err == nil {
		t.Fatal("bad url")
	}
	k.s.GWURL = "https://127.0.0.1:1"
	k.s.at = time.Time{}
	rr = send(h, "GET", "/api/snapshot", "", nil)
	_ = json.Unmarshal(rr.Body.Bytes(), &snap)
	if snap.Up["gateway"] {
		t.Fatal("gateway down not detected")
	}
}

// TC-DSH-013: the mTLS probe passes only on a real server-side refusal. A server that does not demand client
// certificates, an untrusted simulator, a silent listener or a bad target all fail the probe; genuine refusals
// (no certificate / foreign CA) pass repeatedly, whether the OS surfaces them as a TLS alert or a reset.
func TestMTLSProbeVerdicts(t *testing.T) {
	p := testenv.NewPKI(t)
	s := newServer(t)
	s.Gateway, _ = GatewayClient(p.CAPEM)
	d := s.demo
	ns := netsim.New("localhost", "x")
	start := func(tlsCfg *tls.Config) *httptest.Server {
		srv := httptest.NewUnstartedServer(ns.Handler("udr"))
		srv.TLS = tlsCfg
		srv.StartTLS()
		t.Cleanup(srv.Close)
		return srv
	}
	probe := func(base string, cert *tls.Certificate) (string, bool) {
		s.Src.NEProbe = base + "/nudr-dr/v2/subscription-data/imsi-416770000000001/authentication-data/authentication-subscription"
		return d.mtlsProbe(t.Context(), cert)
	}
	if g, pass := probe(start(p.ServerTLS(t, false)).URL, nil); pass || !strings.HasPrefix(g, "accepted") {
		t.Fatal("server without client auth:", g, pass)
	}
	untrusted := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(untrusted.Close)
	if g, pass := probe(untrusted.URL, nil); pass || !strings.Contains(g, "cannot verify") {
		t.Fatal("untrusted simulator:", g, pass)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	if g, pass := probe("https://"+ln.Addr().String(), nil); pass || !strings.Contains(g, "timeout") {
		t.Fatal("silent listener:", g, pass)
	}
	s.Src.NEProbe = "://bad"
	if _, pass := d.mtlsProbe(t.Context(), nil); pass {
		t.Fatal("bad target passed")
	}
	ca, _ := pki.NewCA("rogue-ca", time.Hour)
	bnd, _ := ca.Issue(pki.LeafOpts{CommonName: "adapter", Client: true, Validity: time.Hour})
	rogue, _ := tls.X509KeyPair(bnd.CertPEM, bnd.KeyPEM)
	mtls := start(p.ServerTLS(t, true)).URL
	for i := 0; i < 20; i++ {
		for _, c := range []*tls.Certificate{nil, &rogue} {
			if g, pass := probe(mtls, c); !pass {
				t.Fatalf("run %d cert=%v: genuine refusal not recognised: %s", i, c != nil, g)
			}
		}
	}
}
