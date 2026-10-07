package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"dxps/internal/bus"
	"dxps/internal/contract"
	"dxps/internal/netsim"
	"dxps/internal/registry"
	"dxps/internal/testenv"
)

// env runs every network simulator behind one HTTP/2 mTLS test server and points the seed registry at it.
type env struct {
	ns  *netsim.Netsim
	srv *httptest.Server
	pki *testenv.PKI
	a   *Adapter
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ns := netsim.New("localhost", "x")
	// Remove simulated latency so tests stay fast.
	adm := ns.AdminHandler()
	for name := range ns.Ports() {
		r := httptest.NewRequest("POST", "/faults", strings.NewReader(`{"sim":"`+name+`","latencyMs":0}`))
		r.Header.Set("X-Netsim-Admin", "x")
		adm.ServeHTTP(httptest.NewRecorder(), r)
	}
	routes := map[string]string{"/oauth2/": "nrf", "/nnrf-disc/": "nrf", "/nudr-dr/": "udr", "/ims-pg/": "ims-pg",
		"/restconf/": "restconf", "/usp/": "restconf", "/gsma/": "smdp", "/tmf-api/": "ocs-bss", "/npdb/": "npdb", "/quality-on-demand/": "nef"}
	mux := http.NewServeMux()
	for prefix, sim := range routes {
		mux.Handle(prefix, ns.Handler(sim))
	}
	p := testenv.NewPKI(t)
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = p.ServerTLS(t, true)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: p.ClientTLS(t, true), ForceAttemptHTTP2: true}}
	return &env{ns: ns, srv: srv, pki: p, a: New(testenv.RegistryWithBase(srv.URL), client)}
}

func task(tenant, neTenant, ne, domain, op string, params map[string]any, req string) *contract.NeTask {
	if req == "" {
		req = "null"
	}
	return &contract.NeTask{Tenant: tenant, TaskID: "t-" + op, OrderID: "o-1", NETenant: neTenant, NEID: ne, NECode: ne,
		Domain: domain, Operation: op, Params: params, Request: json.RawMessage(req), IdempotencyKey: "idem-" + op, Priority: contract.P1}
}

const supi = "imsi-416770000000101"

func must(t *testing.T, r *contract.TaskResult, o contract.Outcome, status int, code string) {
	t.Helper()
	if r.Outcome != o || r.NEStatus != status || !strings.HasPrefix(r.NECode, code) {
		t.Fatalf("got outcome=%s status=%d code=%q msg=%q; want %s %d %q", r.Outcome, r.NEStatus, r.NECode, r.Message, o, status, code)
	}
}

// TC-ADP-001: SBA driver fetches an NRF OAuth2 token, sets 3GPP SBI headers and provisions UDR auth data over HTTP/2 mTLS.
func TestSBAProvision(t *testing.T) {
	e := newEnv(t)
	body := `{"authenticationMethod":"5G_AKA","encPermanentKey":"hsm://k/1","encOpcKey":"hsm://k/2"}`
	r := e.a.Execute(context.Background(), task("mvno-alpha", "host-mno", "udr01", "sba", "udr.authSubscription.put", map[string]any{"supi": supi}, body))
	must(t, r, contract.Succeeded, 201, "")
	if r.LatencyUS <= 0 || len(r.Response) == 0 || r.TaskID != "t-udr.authSubscription.put" || r.Tenant != "mvno-alpha" {
		t.Fatalf("%+v", r)
	}
	ex := e.ns.Exchanges(1)[0]
	if ex.Proto != "HTTP/2.0" || ex.Tenant != "mvno-alpha" {
		t.Fatalf("%+v", ex)
	}
	// Token is cached: a second call does not hit the NRF again.
	r = e.a.Execute(context.Background(), task("mvno-alpha", "host-mno", "udr01", "sba", "udr.amData.put", map[string]any{"supi": supi}, `{"gpsis":["msisdn-1"]}`))
	must(t, r, contract.Succeeded, 201, "")
	for _, s := range e.ns.Stats() {
		if s.Name == "nrf" && s.Calls != 1 {
			t.Fatalf("nrf calls = %d, want 1 (token cache)", s.Calls)
		}
	}
	r = e.a.Execute(context.Background(), task("mvno-alpha", "host-mno", "udr01", "sba", "udr.amData.patch", map[string]any{"supi": supi}, `{"rfsp":2}`))
	must(t, r, contract.Succeeded, 204, "")
}

// TC-ADP-002: UDR validation errors are FAILED with the 3GPP cause mapped into the DxPS error code.
func TestSBAValidationFailure(t *testing.T) {
	e := newEnv(t)
	r := e.a.Execute(context.Background(), task("mvno-alpha", "host-mno", "udr01", "sba", "udr.authSubscription.put", map[string]any{"supi": supi},
		`{"authenticationMethod":"5G_AKA","encPermanentKey":"0011","encOpcKey":"hsm://x"}`))
	must(t, r, contract.Failed, 400, "DXPS-3400:MANDATORY_IE_INCORRECT")
	if !strings.Contains(r.Message, "HSM") {
		t.Fatal(r.Message)
	}
}

// TC-ADP-003: IMS ordering precondition and tenant isolation on the shared IMS-PG.
func TestIMS(t *testing.T) {
	e := newEnv(t)
	p := map[string]any{"supi": supi}
	must(t, e.a.Execute(context.Background(), task("mvno-alpha", "host-mno", "imspg01", "ims", "ims.impu.put", p, `{}`)), contract.Failed, 409, "DXPS-3409:PRECONDITION")
	must(t, e.a.Execute(context.Background(), task("mvno-alpha", "host-mno", "imspg01", "ims", "ims.impi.put", p, `{}`)), contract.Succeeded, 200, "")
	must(t, e.a.Execute(context.Background(), task("mvno-beta", "host-mno", "imspg01", "ims", "ims.impi.put", p, `{}`)), contract.Failed, 409, "DXPS-3409:OWNED_BY_OTHER_TENANT")
	must(t, e.a.Execute(context.Background(), task("mvno-alpha", "host-mno", "imspg01", "ims", "ims.impi.delete", p, ``)), contract.Succeeded, 204, "")
}

func l3vpn(bw int) string {
	return `{"ietf-yang-patch:yang-patch":{"patch-id":"p1","edit":[{"edit-id":"e1","operation":"create","target":"/site=S1",` +
		`"value":{"ietf-l3vpn-svc:site":[{"site-id":"S1","service":{"qos":{"svc-input-bandwidth":` + itoa(bw) + `}}}]}}]}}`
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

// TC-ADP-004: RESTCONF YANG-Patch to the PE router: success, capacity rejection (409) and site delete.
func TestNetconf(t *testing.T) {
	e := newEnv(t)
	must(t, e.a.Execute(context.Background(), task("ent-acme", "host-mno", "pe01", "netconf", "netconf.l3vpn.patch", nil, l3vpn(500))), contract.Succeeded, 200, "")
	must(t, e.a.Execute(context.Background(), task("ent-acme", "host-mno", "pe01", "netconf", "netconf.l3vpn.patch", nil, l3vpn(20000))), contract.Failed, 409, "DXPS-3409")
	must(t, e.a.Execute(context.Background(), task("ent-acme", "host-mno", "pe01", "netconf", "netconf.l3vpn.delete", map[string]any{"siteId": "S1"}, ``)), contract.Succeeded, 204, "")
}

// TC-ADP-005: yang-patch-status without "ok" in a 2xx response is a FAILED outcome.
func TestYangPatchStatus(t *testing.T) {
	cases := []struct {
		b    string
		want contract.Outcome
	}{
		{`{}`, contract.Succeeded},
		{`{"ietf-yang-patch:yang-patch-status":{"ok":[null]}}`, contract.Succeeded},
		{`{"ietf-yang-patch:yang-patch-status":{"edit-status":{}}}`, contract.Failed},
	}
	for _, c := range cases {
		var m map[string]any
		_ = json.Unmarshal([]byte(c.b), &m)
		if o, _, _ := (netconf{}).Interpret(200, m); o != c.want {
			t.Fatal(c.b, o)
		}
		if o, _, _ := (access{}).Interpret(200, m); o != c.want {
			t.Fatal("access", c.b, o)
		}
	}
}

// TC-ADP-006: access domain: ONT activation via TR-385 YANG-Patch and CPE config via USP; offline agent fails (7002).
func TestAccess(t *testing.T) {
	e := newEnv(t)
	ont := `{"ietf-yang-patch:yang-patch":{"patch-id":"p","edit":[{"edit-id":"e","operation":"merge","target":"/onu=ALCL1","value":{}}]}}`
	must(t, e.a.Execute(context.Background(), task("host-mno", "host-mno", "olt01", "access", "access.ont.patch", nil, ont)), contract.Succeeded, 200, "")
	must(t, e.a.Execute(context.Background(), task("host-mno", "host-mno", "olt01", "access", "access.ont.delete", map[string]any{"ontSerial": "ALCL1"}, ``)), contract.Succeeded, 204, "")
	usp := `{"header":{"msgId":"m1","msgType":"SET"},"body":{"request":{"set":{}}}}`
	must(t, e.a.Execute(context.Background(), task("host-mno", "host-mno", "usp01", "access", "access.cpe.set", map[string]any{"cpeEndpoint": "os::ABC"}, usp)), contract.Succeeded, 200, "")
	r := e.a.Execute(context.Background(), task("host-mno", "host-mno", "usp01", "access", "access.cpe.set", map[string]any{"cpeEndpoint": "os::OFFLINE-9"}, usp))
	must(t, r, contract.Failed, 200, "DXPS-3USP:7002")
	if r.Message != "agent not reachable" {
		t.Fatal(r.Message)
	}
}

// TC-ADP-007: eSIM ES2+ success and business failures reported inside HTTP 200 are interpreted as FAILED.
func TestESIM(t *testing.T) {
	e := newEnv(t)
	eid := "89049032000000000000000000000101"
	r := e.a.Execute(context.Background(), task("mvno-alpha", "host-mno", "smdp01", "esim", "esim.downloadOrder", nil, `{"eid":"`+eid+`"}`))
	must(t, r, contract.Succeeded, 200, "")
	var dl map[string]any
	_ = json.Unmarshal(r.Response, &dl)
	must(t, e.a.Execute(context.Background(), task("mvno-alpha", "host-mno", "smdp01", "esim", "esim.confirmOrder", nil, `{"iccid":"`+dl["iccid"].(string)+`"}`)), contract.Succeeded, 200, "")
	must(t, e.a.Execute(context.Background(), task("mvno-alpha", "host-mno", "smdp01", "esim", "esim.downloadOrder", nil, `{"eid":"89049032000000000000000000000998"}`)),
		contract.Failed, 200, "DXPS-3ES2:8.1.1-3.8")
	if o, c, _ := (esim{}).Interpret(200, map[string]any{}); o != contract.Failed || c != "DXPS-3ES2:missing-status" {
		t.Fatal(o, c)
	}
	if o, _, _ := (esim{}).Interpret(200, map[string]any{"header": map[string]any{"functionExecutionStatus": map[string]any{"status": "Executed-WithWarning"}}}); o != contract.Succeeded {
		t.Fatal(o)
	}
}

// TC-ADP-008: BSS account (TMF666, X-Request-ID = idempotency key), product, NPDB port and CAMARA QoD session.
func TestBSSAndExposure(t *testing.T) {
	e := newEnv(t)
	acct := `{"name":"a","characteristic":[{"name":"msisdn","value":"14165550101"}]}`
	tk := task("mvno-beta", "host-mno", "ocs01", "bss", "bss.account.create", nil, acct)
	must(t, e.a.Execute(context.Background(), tk), contract.Succeeded, 201, "")
	must(t, e.a.Execute(context.Background(), tk), contract.Succeeded, 201, "") // replay with same X-Request-ID
	must(t, e.a.Execute(context.Background(), task("mvno-beta", "host-mno", "ocs01", "bss", "bss.bundle.create", nil, acct)), contract.Succeeded, 201, "")
	must(t, e.a.Execute(context.Background(), task("mvno-beta", "host-mno", "ocs01", "bss", "bss.account.delete", map[string]any{"msisdn": "14165550101"}, ``)), contract.Succeeded, 204, "")
	own := `{"name":"a","characteristic":[{"name":"msisdn","value":"14165550202"}]}`
	must(t, e.a.Execute(context.Background(), task("mvno-alpha", "mvno-alpha", "ocs-alpha", "bss", "bss.account.create", nil, own)), contract.Succeeded, 201, "")
	must(t, e.a.Execute(context.Background(), task("mvno-alpha", "host-mno", "npdb01", "bss", "npdb.port.put", map[string]any{"msisdn": "14165550101"}, `{"msisdn":"14165550101","routingNumber":"D077"}`)), contract.Succeeded, 200, "")
	must(t, e.a.Execute(context.Background(), task("mvno-alpha", "host-mno", "npdb01", "bss", "npdb.port.delete", map[string]any{"msisdn": "14165550101"}, ``)), contract.Succeeded, 204, "")
	must(t, e.a.Execute(context.Background(), task("ent-acme", "host-mno", "nef01", "exposure", "camara.qod.create", nil, `{"device":{"phoneNumber":"+14165550101"},"qosProfile":"QOS_L"}`)), contract.Succeeded, 201, "")
	must(t, e.a.Execute(context.Background(), task("ent-acme", "host-mno", "nef01", "exposure", "camara.qod.delete", map[string]any{"msisdn": "14165550101"}, ``)), contract.Succeeded, 204, "")
}

// TC-ADP-009: NE 503 with Retry-After is RETRYABLE and carries the delay; 997 recovers on the third attempt.
func TestRetryable(t *testing.T) {
	e := newEnv(t)
	r := e.a.Execute(context.Background(), task("mvno-alpha", "host-mno", "npdb01", "bss", "npdb.port.put", map[string]any{"msisdn": "14165550999"}, `{"msisdn":"14165550999"}`))
	must(t, r, contract.Retryable, 503, "DXPS-3503:NF_CONGESTION")
	if r.RetryAfter != time.Second {
		t.Fatal(r.RetryAfter)
	}
	tk := task("mvno-alpha", "host-mno", "npdb01", "bss", "npdb.port.put", map[string]any{"msisdn": "14165550997"}, `{"msisdn":"14165550997"}`)
	for i, want := range []contract.Outcome{contract.Retryable, contract.Retryable, contract.Succeeded} {
		if r := e.a.Execute(context.Background(), tk); r.Outcome != want {
			t.Fatalf("attempt %d: %s", i, r.Outcome)
		}
	}
}

// TC-ADP-010: SPI guard rails: unsupported op, unknown NE, not entitled, bad/missing path parameters (path traversal).
func TestGuards(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must(t, e.a.Execute(ctx, task("host-mno", "host-mno", "udr01", "sba", "udr.drop", nil, `{}`)), contract.Failed, 0, "DXPS-1001")
	must(t, e.a.Execute(ctx, task("host-mno", "host-mno", "udr01", "nosuch", "x", nil, `{}`)), contract.Failed, 0, "DXPS-1001")
	must(t, e.a.Execute(ctx, task("host-mno", "host-mno", "udr99", "sba", "udr.amData.put", nil, `{}`)), contract.Failed, 0, "DXPS-1003")
	must(t, e.a.Execute(ctx, task("mvno-beta", "host-mno", "pe01", "netconf", "netconf.l3vpn.patch", nil, `{}`)), contract.Failed, 0, "DXPS-1005")
	must(t, e.a.Execute(ctx, task("ent-acme", "host-mno", "udr01", "sba", "udr.amData.put", map[string]any{"supi": supi}, `{}`)), contract.Failed, 0, "DXPS-1005")
	for _, v := range []any{"../../admin", "a b", "x?y=1", strings.Repeat("a", 200), "%2e%2e"} {
		must(t, e.a.Execute(ctx, task("host-mno", "host-mno", "npdb01", "bss", "npdb.port.delete", map[string]any{"msisdn": v}, ``)), contract.Failed, 0, "DXPS-1002")
	}
	must(t, e.a.Execute(ctx, task("host-mno", "host-mno", "npdb01", "bss", "npdb.port.delete", nil, ``)), contract.Failed, 0, "DXPS-1002")
	if _, err := expand(&contract.NeTask{}, "/a/{b"); err == nil {
		t.Fatal("unterminated template")
	}
	if _, err := buildRoute(bssRoutes, &contract.NeTask{Operation: "x"}, nil); err == nil {
		t.Fatal("unknown route")
	}
}

// TC-ADP-011: circuit breaker opens after consecutive transport failures and short-circuits with RETRYABLE (5 s).
func TestBreakerAndTransport(t *testing.T) {
	e := newEnv(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	ne, _ := e.a.Registry.Get("host-mno", "npdb01")
	ne.Endpoints[0].BaseURI = dead.URL
	tk := task("host-mno", "host-mno", "npdb01", "bss", "npdb.port.delete", map[string]any{"msisdn": "1"}, ``)
	for i := 0; i < 5; i++ {
		r := e.a.Execute(context.Background(), tk)
		if r.Outcome != contract.Retryable || !strings.HasPrefix(r.Message, "transport:") {
			t.Fatalf("%+v", r)
		}
	}
	r := e.a.Execute(context.Background(), tk)
	if r.Outcome != contract.Retryable || r.RetryAfter != 5*time.Second || !strings.Contains(r.Message, "circuit open") {
		t.Fatalf("%+v", r)
	}
	var open bool
	for _, s := range e.a.Stats() {
		if s.NE == "npdb01" {
			open = s.Breaker == "open" && s.Calls == 5 && s.Errors == 5
		}
	}
	if !open {
		t.Fatal(e.a.Stats())
	}
}

// TC-ADP-012: client without a certificate cannot reach mTLS simulators (transport failure, RETRYABLE).
func TestNoClientCert(t *testing.T) {
	e := newEnv(t)
	e.a.Client = &http.Client{Transport: &http.Transport{TLSClientConfig: e.pki.ClientTLS(t, false)}}
	r := e.a.Execute(context.Background(), task("host-mno", "host-mno", "npdb01", "bss", "npdb.port.delete", map[string]any{"msisdn": "1"}, ``))
	if r.Outcome != contract.Retryable || !strings.Contains(r.Message, "certificate") {
		t.Fatalf("%+v", r)
	}
}

// TC-ADP-013: NRF failures (no NRF, NRF error status, invalid token response) make SBA tasks RETRYABLE.
func TestNRFFailures(t *testing.T) {
	tk := task("host-mno", "host-mno", "udr01", "sba", "udr.amData.put", map[string]any{"supi": supi}, `{}`)
	e := newEnv(t)
	nrf, _ := e.a.Registry.Get("host-mno", "nrf01")
	for _, h := range []http.HandlerFunc{
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) },
		func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"access_token":""}`)) },
	} {
		s := httptest.NewServer(h)
		nrf.Endpoints[0].BaseURI = s.URL
		e.a.Client = s.Client()
		r := e.a.Execute(context.Background(), tk)
		s.Close()
		if r.Outcome != contract.Retryable || !strings.Contains(r.Message, "NRF") {
			t.Fatalf("%+v", r)
		}
	}
	nrf.Endpoints = nil
	if r := e.a.Execute(context.Background(), tk); r.Outcome != contract.Retryable {
		t.Fatalf("%+v", r)
	}
	nrf.Endpoints = []registry.Endpoint{{BaseURI: "http://[::1]:namedport"}}
	if r := e.a.Execute(context.Background(), tk); r.Outcome != contract.Retryable {
		t.Fatalf("%+v", r)
	}
	udr, _ := e.a.Registry.Get("host-mno", "udr01")
	solo := New(registry.New(udr), e.a.Client)
	if r := solo.Execute(context.Background(), tk); r.Outcome != contract.Retryable || !strings.Contains(r.Message, "no NRF") {
		t.Fatalf("%+v", r)
	}
}

// TC-ADP-014: NE without endpoints and a malformed base URI fail cleanly; quota wait honours context cancel.
func TestEndpointErrors(t *testing.T) {
	e := newEnv(t)
	ne, _ := e.a.Registry.Get("host-mno", "imspg01")
	tk := task("host-mno", "host-mno", "imspg01", "ims", "ims.impi.delete", map[string]any{"supi": supi}, ``)
	ne.Endpoints = []registry.Endpoint{{BaseURI: "http://bad host"}}
	if r := e.a.Execute(context.Background(), tk); r.Outcome != contract.Retryable || !strings.HasPrefix(r.Message, "transport") {
		t.Fatalf("%+v", r)
	}
	ne.Endpoints = nil
	must(t, e.a.Execute(context.Background(), tk), contract.Failed, 0, "DXPS-1003")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tk.Tenant = "mvno-alpha" // tenant quota limiter observes the cancelled context
	if r := e.a.Execute(ctx, tk); r.Outcome != contract.Retryable || !strings.HasPrefix(r.Message, "budget") {
		t.Fatalf("%+v", r)
	}
}

// TC-ADP-015: problem() maps 3GPP ProblemDetails, CAMARA and TMF630 errors to DXPS codes; timeouts per tier.
func TestProblemAndTimeout(t *testing.T) {
	code, msg := problem(404, map[string]any{"cause": "DATA_NOT_FOUND", "detail": "nope"})
	if code != "DXPS-3404:DATA_NOT_FOUND" || msg != "nope" {
		t.Fatal(code, msg)
	}
	if code, msg := problem(400, map[string]any{"code": "INVALID_ARGUMENT", "message": "m"}); code != "DXPS-3400:INVALID_ARGUMENT" || msg != "m" {
		t.Fatal(code, msg)
	}
	if code, msg := problem(500, nil); code != "DXPS-3500" || msg != "Internal Server Error" {
		t.Fatal(code, msg)
	}
	if c, _ := problem(400, map[string]any{"reason": strings.Repeat("x", 100)}); len(c) != len("DXPS-3400:")+40 {
		t.Fatal(c)
	}
	if timeout(contract.P0) != 3*time.Second || timeout(contract.P1) != 3*time.Second || timeout(contract.P3) != 10*time.Second {
		t.Fatal("timeouts")
	}
	a := &Adapter{}
	if time.Since(a.now()) > time.Second {
		t.Fatal("now")
	}
}

// TC-ADP-016: Process validates the Kafka record (decode, tenant header/key/payload match) and emits a TaskResult.
func TestProcess(t *testing.T) {
	e := newEnv(t)
	rec := func(hdrTenant, key string, v []byte) *kgo.Record {
		return bus.Record("dxps.task.bss.p1", key, v, map[string]string{contract.HdrTenant: hdrTenant})
	}
	if out := e.a.Process(context.Background(), rec("host-mno", "host-mno|o", []byte("{bad"))); out[0].Topic != contract.TopicDLQ {
		t.Fatal(out[0].Topic)
	}
	tk := task("mvno-alpha", "host-mno", "npdb01", "bss", "npdb.port.delete", map[string]any{"msisdn": "1"}, ``)
	b, _ := json.Marshal(tk)
	for _, r := range []*kgo.Record{rec("mvno-beta", "mvno-alpha|o-1", b), rec("mvno-alpha", "mvno-beta|o-1", b)} {
		out := e.a.Process(context.Background(), r)
		if out[0].Topic != contract.TopicDLQ || !strings.Contains(bus.Header(out[0], "error"), "DXPS-1004") {
			t.Fatalf("cross-tenant record accepted: topic=%s headers=%v", out[0].Topic, out[0].Headers)
		}
	}
	out := e.a.Process(context.Background(), rec("mvno-alpha", "mvno-alpha|o-1", b))
	var res contract.TaskResult
	_ = json.Unmarshal(out[0].Value, &res)
	if out[0].Topic != contract.TopicTaskResult || string(out[0].Key) != "mvno-alpha|o-1" || res.Outcome != contract.Succeeded ||
		bus.Header(out[0], contract.HdrProducer) != "adapter-bss" || bus.Header(out[0], contract.HdrPriority) != "p1" {
		t.Fatalf("%s %+v", out[0].Topic, res)
	}
}

// TC-ADP-017: stats endpoint binds to loopback only and serves per-NE stats.
func TestServeStats(t *testing.T) {
	a := New(testenv.Registry("localhost"), http.DefaultClient)
	for _, addr := range []string{"0.0.0.0:0", ":9202", "bad"} {
		if err := a.serveStats(context.Background(), addr); err == nil {
			t.Fatal("bound", addr)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.serveStats(ctx, "127.0.0.1:0") }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(a.Stats()) != 11 {
		t.Fatal(len(a.Stats()))
	}
	if err := a.serveStats(context.Background(), "127.0.0.1:99999"); err == nil {
		t.Fatal("invalid port")
	}
}
