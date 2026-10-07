package gateway

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dxps/internal/auth"
	"dxps/internal/catalog"
	"dxps/internal/ids"
	"dxps/internal/secretbox"
	"dxps/internal/seed"
	"dxps/internal/store"
	"dxps/internal/tenant"
	"dxps/internal/testenv/pgenv"
)

const (
	tA = "zz-test-gw"  // test tenants keep integration data apart from the demo tenants
	tB = "zz-test-gw2" //
)

var allScopes = []string{auth.ScopeOrderWrite, auth.ScopeOrderRead, auth.ScopeHubWrite}

type fixture struct {
	g   *Gateway
	h   http.Handler
	sig *auth.Signer
}

func newGW(t *testing.T, st *store.Store) *fixture {
	t.Helper()
	sig, err := auth.NewSigner([]byte("0123456789abcdef0123456789abcdef"), "dxps-test", "dxps-api")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	rand.Read(key)
	box, _ := secretbox.New(key)
	ts := append(seed.Tenants(),
		tenant.Tenant{ID: tA, Name: "test A", Type: tenant.FullMVNO, SLAWeight: 1, QuotaTPS: 1000, Status: tenant.Active},
		tenant.Tenant{ID: tB, Name: "test B", Type: tenant.LightMVNO, SLAWeight: 1, QuotaTPS: 1000, Status: tenant.Active},
		tenant.Tenant{ID: "zz-slow", Name: "slow", Type: tenant.LightMVNO, SLAWeight: 1, QuotaTPS: 1, Status: tenant.Active})
	g := &Gateway{Store: st, Catalog: cat, Tenants: tenant.NewRegistry(ts...), Signer: sig, Box: box,
		HubAllow: []string{"localhost:9109", "hooks.example:443"}}
	if st != nil {
		g.Pub = markPublished{st}
	}
	return &fixture{g: g, h: g.Handler(), sig: sig}
}

// markPublished stands in for the Kafka fast path so the outbox relay does not pick up test rows.
type markPublished struct{ s *store.Store }

func (m markPublished) Publish(ctx context.Context, rows []*store.OutboxRow) error {
	return m.s.MarkPublished(ctx, rows)
}

func (f *fixture) token(t *testing.T, ten string, scopes ...string) string {
	t.Helper()
	tok, err := f.sig.Issue("client-"+ten, ten, scopes, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

type req struct {
	method, path, body, tok string
	hdr                     map[string]string
}

func (f *fixture) do(t *testing.T, r req) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	hr := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
	if r.body != "" {
		hr.Header.Set("Content-Type", "application/json")
	}
	if r.tok != "" {
		hr.Header.Set("Authorization", "Bearer "+r.tok)
	}
	for k, v := range r.hdr {
		if v == "" {
			hr.Header.Del(k)
		} else {
			hr.Header.Set(k, v)
		}
	}
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, hr)
	var m map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	return rr, m
}

func order(supi, msisdn string, extra ...string) string {
	ext := ""
	if len(extra) > 0 {
		ext = extra[0]
	}
	return fmt.Sprintf(`{"@type":"ServiceOrder","externalId":%q,"channel":[{"name":"SELFCARE"}],"serviceOrderItem":[{"id":"1","action":"add",
	"service":{"serviceSpecification":{"id":"CreateSubscriber5G"},"serviceCharacteristic":[
	{"name":"supi","value":%q},{"name":"msisdn","value":%q},{"name":"plan","value":"5G-100GB"}]}}]}`, ext, supi, msisdn)
}

func code(m map[string]any) string { s, _ := m["code"].(string); return s }

// TC-GW-001: security headers on every response; /healthz is unauthenticated.
func TestSecurityHeaders(t *testing.T) {
	f := newGW(t, nil)
	rr, _ := f.do(t, req{method: "GET", path: "/healthz"})
	if rr.Code != 200 || rr.Body.String() != "ok" {
		t.Fatal(rr.Code)
	}
	for _, p := range []string{"/healthz", BasePath + "/serviceOrder"} {
		rr, _ := f.do(t, req{method: "GET", path: p})
		for k, v := range map[string]string{"Strict-Transport-Security": "max-age=31536000; includeSubDomains", "X-Content-Type-Options": "nosniff",
			"Cache-Control": "no-store", "Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'", "Referrer-Policy": "no-referrer"} {
			if rr.Header().Get(k) != v {
				t.Fatal(p, k, rr.Header().Get(k))
			}
		}
	}
}

// TC-GW-002: authentication: missing, malformed, forged, expired and wrong-audience tokens are 401 with WWW-Authenticate.
func TestAuthentication(t *testing.T) {
	f := newGW(t, nil)
	other, _ := auth.NewSigner([]byte("ffffffffffffffffffffffffffffffff"), "dxps-test", "dxps-api")
	forged, _ := other.Issue("x", tA, allScopes, time.Minute)
	wrongAud, _ := auth.NewSigner([]byte("0123456789abcdef0123456789abcdef"), "dxps-test", "other-api")
	aud, _ := wrongAud.Issue("x", tA, allScopes, time.Minute)
	old, _ := auth.NewSigner([]byte("0123456789abcdef0123456789abcdef"), "dxps-test", "dxps-api")
	old.SetClock(func() time.Time { return time.Now().Add(-time.Hour) })
	expired, _ := old.Issue("x", tA, allScopes, time.Minute)
	for name, tok := range map[string]string{"none": "", "garbage": "abc.def.ghi", "forged": forged, "audience": aud, "expired": expired} {
		rr, m := f.do(t, req{method: "GET", path: BasePath + "/serviceOrder", tok: tok})
		if rr.Code != 401 || code(m) != "DXPS-1000" || !strings.HasPrefix(rr.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Fatal(name, rr.Code, m)
		}
	}
	hr := httptest.NewRequest("GET", BasePath+"/serviceOrder", nil)
	hr.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, hr)
	if rr.Code != 401 {
		t.Fatal("basic auth accepted")
	}
}

// TC-GW-003: authorization: scopes per operation, X-Tenant needs dxps:tenant:any, unknown/suspended tenants are 403.
func TestAuthorization(t *testing.T) {
	f := newGW(t, nil)
	ro := f.token(t, tA, auth.ScopeOrderRead)
	cases := []struct {
		name string
		r    req
		want int
	}{
		{"write without scope", req{method: "POST", path: BasePath + "/serviceOrder", body: "{}", tok: ro}, 403},
		{"cancel without scope", req{method: "POST", path: BasePath + "/cancelServiceOrder", body: "{}", tok: ro}, 403},
		{"hub without scope", req{method: "POST", path: BasePath + "/hub", body: "{}", tok: ro}, 403},
		{"hub delete without scope", req{method: "DELETE", path: BasePath + "/hub/x", tok: ro}, 403},
		{"read with only write", req{method: "GET", path: BasePath + "/serviceOrder", tok: f.token(t, tA, auth.ScopeOrderWrite)}, 403},
		{"X-Tenant without tenant:any", req{method: "GET", path: BasePath + "/serviceOrder", tok: ro, hdr: map[string]string{"X-Tenant": tB}}, 403},
		{"X-Tenant invalid", req{method: "GET", path: BasePath + "/serviceOrder", tok: f.token(t, "host-mno", auth.ScopeOrderRead, auth.ScopeTenantAny),
			hdr: map[string]string{"X-Tenant": "../etc"}}, 403},
		{"X-Tenant unknown", req{method: "GET", path: BasePath + "/serviceOrder", tok: f.token(t, "host-mno", auth.ScopeOrderRead, auth.ScopeTenantAny),
			hdr: map[string]string{"X-Tenant": "zz-nobody"}}, 403},
		{"unknown tenant", req{method: "GET", path: BasePath + "/serviceOrder", tok: f.token(t, "zz-nobody", auth.ScopeOrderRead)}, 403},
		{"suspended tenant", req{method: "GET", path: BasePath + "/serviceOrder", tok: f.token(t, "mvno-gamma", auth.ScopeOrderRead)}, 403},
		{"unknown route", req{method: "GET", path: BasePath + "/admin", tok: ro}, 404},
	}
	for _, c := range cases {
		if rr, _ := f.do(t, c.r); rr.Code != c.want {
			t.Fatal(c.name, rr.Code, rr.Body)
		}
	}
}

// TC-GW-004: per-tenant rate limit returns 429 with Retry-After; limiter follows quota changes.
func TestRateLimit(t *testing.T) {
	f := newGW(t, nil)
	tok := f.token(t, "zz-slow", auth.ScopeOrderRead)
	r := req{method: "GET", path: BasePath + "/serviceOrder?limit=0", tok: tok}
	f.do(t, r)
	rr, m := f.do(t, r)
	if rr.Code != 429 || code(m) != "DXPS-1006" || rr.Header().Get("Retry-After") != "1" {
		t.Fatal(rr.Code, m)
	}
	l1 := f.g.limiter(tenant.Tenant{ID: "q", QuotaTPS: 5})
	if f.g.limiter(tenant.Tenant{ID: "q", QuotaTPS: 5}) != l1 || f.g.limiter(tenant.Tenant{ID: "q", QuotaTPS: 50}) == l1 {
		t.Fatal("limiter cache")
	}
}

// TC-GW-005: request hygiene: Idempotency-Key format, JSON content type, unknown fields, trailing data, 256 KiB body cap.
func TestRequestHygiene(t *testing.T) {
	f := newGW(t, nil)
	tok := f.token(t, tA, allScopes...)
	ok := map[string]string{"Idempotency-Key": "idem-12345678"}
	big := `{"description":"` + strings.Repeat("a", MaxBody) + `"}`
	cases := []struct {
		name string
		r    req
		want int
	}{
		{"no idempotency key", req{method: "POST", path: BasePath + "/serviceOrder", body: "{}", tok: tok}, 400},
		{"short idempotency key", req{method: "POST", path: BasePath + "/serviceOrder", body: "{}", tok: tok, hdr: map[string]string{"Idempotency-Key": "abc"}}, 400},
		{"bad chars idempotency key", req{method: "POST", path: BasePath + "/serviceOrder", body: "{}", tok: tok, hdr: map[string]string{"Idempotency-Key": "abc def ghi"}}, 400},
		{"wrong content type", req{method: "POST", path: BasePath + "/serviceOrder", body: "{}", tok: tok, hdr: map[string]string{"Idempotency-Key": "idem-12345678", "Content-Type": "text/plain"}}, 400},
		{"unknown field", req{method: "POST", path: BasePath + "/serviceOrder", body: `{"evil":1}`, tok: tok, hdr: ok}, 400},
		{"trailing data", req{method: "POST", path: BasePath + "/serviceOrder", body: `{} {}`, tok: tok, hdr: ok}, 400},
		{"not json", req{method: "POST", path: BasePath + "/serviceOrder", body: `[`, tok: tok, hdr: ok}, 400},
		{"too large", req{method: "POST", path: BasePath + "/serviceOrder", body: big, tok: tok, hdr: ok}, 413},
		{"cancel bad body", req{method: "POST", path: BasePath + "/cancelServiceOrder", body: `{"x":1}`, tok: tok}, 400},
		{"cancel bad id", req{method: "POST", path: BasePath + "/cancelServiceOrder", body: `{"serviceOrder":{"id":"1 OR 1=1"}}`, tok: tok}, 400},
		{"get bad id", req{method: "GET", path: BasePath + "/serviceOrder/'%20OR%201=1--", tok: tok}, 404},
		{"list bad state", req{method: "GET", path: BasePath + "/serviceOrder?state=x'%20OR%20'1'='1", tok: tok}, 400},
		{"list bad limit", req{method: "GET", path: BasePath + "/serviceOrder?limit=1000", tok: tok}, 400},
		{"list bad limit 0", req{method: "GET", path: BasePath + "/serviceOrder?limit=0", tok: tok}, 400},
		{"list bad offset", req{method: "GET", path: BasePath + "/serviceOrder?offset=-1", tok: tok}, 400},
		{"list huge offset", req{method: "GET", path: BasePath + "/serviceOrder?offset=999999999", tok: tok}, 400},
		{"hub bad body", req{method: "POST", path: BasePath + "/hub", body: `{"callback":1}`, tok: tok}, 400},
		{"hub delete bad id", req{method: "DELETE", path: BasePath + "/hub/abc", tok: tok}, 404},
	}
	for _, c := range cases {
		if rr, _ := f.do(t, c.r); rr.Code != c.want {
			t.Fatal(c.name, rr.Code, rr.Body)
		}
	}
}

// TC-GW-006: TMF641 order validation: items, ids, actions, catalog, characteristics, CEL/JSON-schema params, dependsOn graph.
func TestValidateOrder(t *testing.T) {
	f := newGW(t, nil)
	item := func(id, action, spec string, chars string, deps ...string) OrderItem {
		var cs []Characteristic
		_ = json.Unmarshal([]byte(chars), &cs)
		it := OrderItem{ID: id, Action: action, Service: ServiceRefOrValue{ServiceSpecification: SpecRef{ID: spec}, ServiceCharacteristic: cs}}
		for _, d := range deps {
			it.ServiceOrderItemRelationship = append(it.ServiceOrderItemRelationship, ItemRelationship{RelationshipType: "dependsOn", OrderItem: ItemRef{ItemID: d}})
		}
		return it
	}
	good := `[{"name":"supi","value":"imsi-416770000000101"},{"name":"msisdn","value":"14165550101"},{"name":"plan","value":"5G-100GB"}]`
	vonr := `[{"name":"supi","value":"imsi-416770000000101"},{"name":"msisdn","value":"14165550101"}]`
	many := make([]OrderItem, MaxItems+1)
	for i := range many {
		many[i] = item(fmt.Sprint(i), "add", "CreateSubscriber5G", good)
	}
	var chars []string
	for i := 0; i <= MaxChars; i++ {
		chars = append(chars, fmt.Sprintf(`{"name":"c%d","value":1}`, i))
	}
	cases := []struct {
		name string
		r    ServiceOrderReq
		code string
	}{
		{"no items", ServiceOrderReq{}, "DXPS-1002"},
		{"too many items", ServiceOrderReq{ServiceOrderItem: many}, "DXPS-1002"},
		{"bad externalId", ServiceOrderReq{ExternalID: "<script>", ServiceOrderItem: []OrderItem{item("1", "add", "CreateSubscriber5G", good)}}, "DXPS-1002"},
		{"bad priority", ServiceOrderReq{Priority: "9", ServiceOrderItem: []OrderItem{item("1", "add", "CreateSubscriber5G", good)}}, "DXPS-1002"},
		{"bad item id", ServiceOrderReq{ServiceOrderItem: []OrderItem{item("a/b", "add", "CreateSubscriber5G", good)}}, "DXPS-1002"},
		{"duplicate item", ServiceOrderReq{ServiceOrderItem: []OrderItem{item("1", "add", "CreateSubscriber5G", good), item("1", "add", "CreateSubscriber5G", good)}}, "DXPS-1002"},
		{"bad action", ServiceOrderReq{ServiceOrderItem: []OrderItem{item("1", "drop", "CreateSubscriber5G", good)}}, "DXPS-1002"},
		{"unknown spec", ServiceOrderReq{ServiceOrderItem: []OrderItem{item("1", "add", "RootShell", good)}}, "DXPS-1001"},
		{"too many chars", ServiceOrderReq{ServiceOrderItem: []OrderItem{item("1", "add", "CreateSubscriber5G", "["+strings.Join(chars, ",")+"]")}}, "DXPS-1002"},
		{"bad char name", ServiceOrderReq{ServiceOrderItem: []OrderItem{item("1", "add", "CreateSubscriber5G", `[{"name":"$where","value":1}]`)}}, "DXPS-1002"},
		{"dup char", ServiceOrderReq{ServiceOrderItem: []OrderItem{item("1", "add", "CreateSubscriber5G", `[{"name":"a","value":1},{"name":"a","value":2}]`)}}, "DXPS-1002"},
		{"bad char value", ServiceOrderReq{ServiceOrderItem: []OrderItem{{ID: "1", Action: "add", Service: ServiceRefOrValue{ServiceSpecification: SpecRef{ID: "CreateSubscriber5G"},
			ServiceCharacteristic: []Characteristic{{Name: "supi", Value: json.RawMessage(`{`)}}}}}}, "DXPS-1002"},
		{"param pattern", ServiceOrderReq{ServiceOrderItem: []OrderItem{item("1", "add", "CreateSubscriber5G", `[{"name":"supi","value":"imsi-1; DROP TABLE"},{"name":"msisdn","value":"1"},{"name":"plan","value":"x"}]`)}}, "DXPS-1002"},
		{"missing param", ServiceOrderReq{ServiceOrderItem: []OrderItem{item("1", "add", "CreateSubscriber5G", `[{"name":"supi","value":"imsi-416770000000101"}]`)}}, "DXPS-1002"},
		{"dependsOn unknown", ServiceOrderReq{ServiceOrderItem: []OrderItem{item("1", "add", "CreateSubscriber5G", good, "9")}}, "DXPS-1002"},
		{"dependsOn self", ServiceOrderReq{ServiceOrderItem: []OrderItem{item("1", "add", "CreateSubscriber5G", good, "1")}}, "DXPS-1002"},
		{"cycle", ServiceOrderReq{ServiceOrderItem: []OrderItem{item("1", "add", "CreateSubscriber5G", good, "2"), item("2", "add", "AddVoNR", vonr, "1")}}, "DXPS-1002"},
	}
	for _, c := range cases {
		_, _, st, cd, err := f.g.validateOrder(tA, &c.r)
		if err == nil || st != 400 || cd != c.code {
			t.Fatal(c.name, st, cd, err)
		}
	}
	r := ServiceOrderReq{Priority: "p0", ServiceOrderItem: []OrderItem{item("sub", "add", "CreateSubscriber5G", good), item("volte", "add", "AddVoNR", vonr, "sub")}}
	r.ServiceOrderItem[1].ServiceOrderItemRelationship = append(r.ServiceOrderItem[1].ServiceOrderItemRelationship, ItemRelationship{RelationshipType: "relatedTo", OrderItem: ItemRef{ItemID: "zz"}})
	items, prio, _, _, err := f.g.validateOrder(tA, &r)
	if err != nil || len(items) != 2 || prio != 0 || items[1].deps[0] != "sub" || items[0].keys[0] != "supi:imsi-416770000000101" {
		t.Fatal(items, prio, err)
	}
	r.Priority = ""
	if _, prio, _, _, _ := f.g.validateOrder(tA, &r); prio != 1 {
		t.Fatal("default priority from spec", prio)
	}
}

// TC-GW-007: TMF688 callback allow-list (SSRF): https only, exact host:port, no userinfo/fragment, length cap.
func TestCallbackAllowed(t *testing.T) {
	g := &Gateway{HubAllow: []string{"localhost:9109", "hooks.example:443"}}
	cases := map[string]bool{
		"https://localhost:9109/hooks/a": true, "https://hooks.example/x": true, "https://HOOKS.example:443/x": true,
		"http://localhost:9109/": false, "https://localhost:22/": false, "https://169.254.169.254/latest/meta-data": false,
		"https://a:b@localhost:9109/": false, "https://localhost:9109/#x": false, "https:///x": false, "gopher://localhost:9109": false,
		"https://localhost:9109/" + strings.Repeat("a", 600): false, "%": false, "https://localhost:9109.evil.example/": false,
	}
	for cb, want := range cases {
		if g.callbackAllowed(cb) != want {
			t.Fatal(cb)
		}
	}
	if s := (&Gateway{HubAllow: []string{"b", "a"}}).SortedAllow(); s[0] != "a" {
		t.Fatal(s)
	}
}

// TC-GW-008: hub registration input validation happens before any storage access.
func TestHubValidation(t *testing.T) {
	f := newGW(t, nil)
	tok := f.token(t, tA, auth.ScopeHubWrite)
	for name, body := range map[string]string{
		"not allow-listed": `{"callback":"https://evil.example/x"}`,
		"query too long":   `{"callback":"https://localhost:9109/h","query":"` + strings.Repeat("a", 300) + `"}`,
		"short secret":     `{"callback":"https://localhost:9109/h","secret":"short"}`,
		"long secret":      `{"callback":"https://localhost:9109/h","secret":"` + strings.Repeat("s", 200) + `"}`,
	} {
		if rr, _ := f.do(t, req{method: "POST", path: BasePath + "/hub", body: body, tok: tok}); rr.Code != 400 {
			t.Fatal(name, rr.Code)
		}
	}
}

// TC-GW-009: panics inside handlers are recovered into a TMF630 500 without leaking internals.
func TestPanicRecovery(t *testing.T) {
	f := newGW(t, nil) // nil store: reaching storage panics
	tok := f.token(t, tA, allScopes...)
	rr, m := f.do(t, req{method: "GET", path: BasePath + "/serviceOrder", tok: tok})
	if rr.Code != 500 || code(m) != "DXPS-9000" || strings.Contains(rr.Body.String(), "nil pointer") {
		t.Fatal(rr.Code, rr.Body)
	}
	if f.g.log() == nil {
		t.Fatal("log")
	}
}

// TC-GW-010: toTMF maps store rows to TMF641 including item states and error messages.
func TestToTMF(t *testing.T) {
	items, _ := json.Marshal([]OrderItem{{ID: "1", Action: "add"}, {ID: "2", Action: "add"}})
	o := &store.OrderView{ID: "x", State: "failed", Priority: 1, Items: items, ErrorCode: "DXPS-3409", Error: "conflict",
		ItemStates: []store.ItemState{{ItemID: "1", State: "completed"}, {ItemID: "2", State: "failed", ErrorCode: "DXPS-3400", Error: "bad"}}}
	so := toTMF(o)
	if so.ServiceOrderItem[0].State != "completed" || so.ServiceOrderItem[1].State != "failed" || len(so.ErrorMessage) != 1 || so.ErrorMessage[0].Code != "DXPS-3400" {
		t.Fatalf("%+v", so)
	}
	o2 := toTMF(&store.OrderView{ID: "y", ErrorCode: "DXPS-9", Error: "e"})
	if len(o2.ServiceOrderItem) != 0 || o2.ServiceOrderItem == nil || o2.ErrorMessage[0].Reason != "order" {
		t.Fatalf("%+v", o2)
	}
}

// ---------------------------------------------------------------- integration (PostgreSQL)

func unique() (string, string) {
	n := time.Now().UnixNano() % 1_000_000_000
	return fmt.Sprintf("imsi-41677%010d", n), fmt.Sprintf("1416%09d", n)
}

// TC-GW-011 (integration): create order 201, idempotent replay 200, key reuse with different body 409, get, list.
func TestOrderLifecycleIntegration(t *testing.T) {
	st, _ := pgenv.Store(t)
	f := newGW(t, st)
	tok := f.token(t, tA, allScopes...)
	supi, msisdn := unique()
	idem := "idem-" + ids.NewString()
	body := order(supi, msisdn, "EXT-"+msisdn)
	h := map[string]string{"Idempotency-Key": idem}
	rr, m := f.do(t, req{method: "POST", path: BasePath + "/serviceOrder", body: body, tok: tok, hdr: h})
	if rr.Code != 201 || m["state"] != "acknowledged" || !strings.HasPrefix(rr.Header().Get("Location"), BasePath+"/serviceOrder/") {
		t.Fatal(rr.Code, rr.Body)
	}
	id := m["id"].(string)
	if id != ids.Derive("order", tA, idem).String() {
		t.Fatal("order id must be derived from tenant + idempotency key")
	}
	rr, m = f.do(t, req{method: "POST", path: BasePath + "/serviceOrder", body: body, tok: tok, hdr: h})
	if rr.Code != 200 || rr.Header().Get("Idempotent-Replayed") != "true" || m["id"] != id {
		t.Fatal("replay", rr.Code, rr.Body)
	}
	s2, m2 := unique()
	if rr, m := f.do(t, req{method: "POST", path: BasePath + "/serviceOrder", body: order(s2, m2), tok: tok, hdr: h}); rr.Code != 409 || code(m) != "DXPS-1008" {
		t.Fatal("key reuse", rr.Code, rr.Body)
	}
	if rr, m := f.do(t, req{method: "GET", path: BasePath + "/serviceOrder/" + id, tok: tok}); rr.Code != 200 || m["externalId"] != "EXT-"+msisdn {
		t.Fatal(rr.Code, rr.Body)
	}
	rr, _ = f.do(t, req{method: "GET", path: BasePath + "/serviceOrder?limit=5&offset=0&state=acknowledged", tok: tok})
	if rr.Code != 200 || rr.Header().Get("X-Result-Count") == "" {
		t.Fatal(rr.Code, rr.Body)
	}
	if rr, _ := f.do(t, req{method: "GET", path: BasePath + "/serviceOrder/" + ids.NewString(), tok: tok}); rr.Code != 404 {
		t.Fatal(rr.Code)
	}
	// cancel: first succeeds (201), second conflicts (409), unknown 404
	cancel := `{"cancellationReason":"test","serviceOrder":{"id":"` + id + `"}}`
	if rr, _ := f.do(t, req{method: "POST", path: BasePath + "/cancelServiceOrder", body: cancel, tok: tok}); rr.Code != 201 {
		t.Fatal("cancel", rr.Code, rr.Body)
	}
	if rr, m := f.do(t, req{method: "POST", path: BasePath + "/cancelServiceOrder", body: cancel, tok: tok}); rr.Code != 409 || code(m) != "DXPS-1009" {
		t.Fatal("cancel twice", rr.Code, rr.Body)
	}
	if rr, _ := f.do(t, req{method: "POST", path: BasePath + "/cancelServiceOrder", body: `{"serviceOrder":{"id":"` + ids.NewString() + `"}}`, tok: tok}); rr.Code != 404 {
		t.Fatal(rr.Code)
	}
}

// TC-GW-012 (integration): tenant isolation through RLS: tenant B cannot read, list or cancel tenant A's order;
// the same Idempotency-Key in two tenants yields two different orders.
func TestTenantIsolationIntegration(t *testing.T) {
	st, _ := pgenv.Store(t)
	f := newGW(t, st)
	ta, tb := f.token(t, tA, allScopes...), f.token(t, tB, allScopes...)
	supi, msisdn := unique()
	idem := "idem-" + ids.NewString()
	rr, m := f.do(t, req{method: "POST", path: BasePath + "/serviceOrder", body: order(supi, msisdn), tok: ta, hdr: map[string]string{"Idempotency-Key": idem}})
	if rr.Code != 201 {
		t.Fatal(rr.Code, rr.Body)
	}
	id := m["id"].(string)
	if rr, _ := f.do(t, req{method: "GET", path: BasePath + "/serviceOrder/" + id, tok: tb}); rr.Code != 404 {
		t.Fatal("cross-tenant read", rr.Code)
	}
	rr, _ = f.do(t, req{method: "GET", path: BasePath + "/serviceOrder?limit=100", tok: tb})
	if strings.Contains(rr.Body.String(), id) {
		t.Fatal("cross-tenant list")
	}
	if rr, _ := f.do(t, req{method: "POST", path: BasePath + "/cancelServiceOrder", body: `{"serviceOrder":{"id":"` + id + `"}}`, tok: tb}); rr.Code != 404 {
		t.Fatal("cross-tenant cancel", rr.Code)
	}
	rr, m = f.do(t, req{method: "POST", path: BasePath + "/serviceOrder", body: order(supi, msisdn), tok: tb, hdr: map[string]string{"Idempotency-Key": idem}})
	if rr.Code != 201 || m["id"] == id {
		t.Fatal("same key in another tenant", rr.Code, rr.Body)
	}
}

// TC-GW-013 (integration): host operator acting for a tenant via X-Tenant (dxps:tenant:any) sees that tenant's data only.
func TestActingForIntegration(t *testing.T) {
	st, _ := pgenv.Store(t)
	f := newGW(t, st)
	op := f.token(t, "host-mno", append(allScopes, auth.ScopeTenantAny)...)
	supi, msisdn := unique()
	rr, m := f.do(t, req{method: "POST", path: BasePath + "/serviceOrder", body: order(supi, msisdn), tok: op,
		hdr: map[string]string{"Idempotency-Key": "idem-" + ids.NewString(), "X-Tenant": tA}})
	if rr.Code != 201 {
		t.Fatal(rr.Code, rr.Body)
	}
	if rr, _ := f.do(t, req{method: "GET", path: BasePath + "/serviceOrder/" + m["id"].(string), tok: f.token(t, tA, allScopes...)}); rr.Code != 200 {
		t.Fatal("order must belong to the acted-for tenant", rr.Code)
	}
}

// TC-GW-014 (integration): hub create (generated secret returned once, sealed at rest), delete, cross-tenant delete 404.
func TestHubIntegration(t *testing.T) {
	st, _ := pgenv.Store(t)
	f := newGW(t, st)
	ta, tb := f.token(t, tA, allScopes...), f.token(t, tB, allScopes...)
	rr, m := f.do(t, req{method: "POST", path: BasePath + "/hub", body: `{"callback":"https://localhost:9109/hooks/zz","query":"state=completed"}`, tok: ta})
	if rr.Code != 201 || len(m["secret"].(string)) < 32 {
		t.Fatal(rr.Code, rr.Body)
	}
	id := m["id"].(string)
	rr, m = f.do(t, req{method: "POST", path: BasePath + "/hub", body: `{"callback":"https://localhost:9109/hooks/zz2","secret":"0123456789abcdef"}`, tok: ta})
	if rr.Code != 201 || m["secret"] != nil {
		t.Fatal("client-supplied secret must not be echoed", rr.Body)
	}
	id2 := m["id"].(string)
	if rr, _ := f.do(t, req{method: "DELETE", path: BasePath + "/hub/" + id, tok: tb}); rr.Code != 404 {
		t.Fatal("cross-tenant delete", rr.Code)
	}
	for _, x := range []string{id, id2} {
		if rr, _ := f.do(t, req{method: "DELETE", path: BasePath + "/hub/" + x, tok: ta}); rr.Code != 204 {
			t.Fatal(rr.Code)
		}
	}
	if rr, _ := f.do(t, req{method: "DELETE", path: BasePath + "/hub/" + id, tok: ta}); rr.Code != 404 {
		t.Fatal(rr.Code)
	}
}

// TC-GW-015 (integration): storage failures surface as 503 DXPS-9001 (no internal error text).
func TestStoreUnavailableIntegration(t *testing.T) {
	st, cfg := pgenv.Store(t)
	ctx := context.Background()
	closed, err := store.Open(ctx, cfg.PGApp, cfg.PGOps)
	if err != nil {
		t.Skip(err)
	}
	closed.Close()
	_ = st
	f := newGW(t, closed)
	f.g.Pub = nil
	tok := f.token(t, tA, allScopes...)
	supi, msisdn := unique()
	for _, r := range []req{
		{method: "POST", path: BasePath + "/serviceOrder", body: order(supi, msisdn), tok: tok, hdr: map[string]string{"Idempotency-Key": "idem-" + ids.NewString()}},
		{method: "GET", path: BasePath + "/serviceOrder", tok: tok},
		{method: "GET", path: BasePath + "/serviceOrder/" + ids.NewString(), tok: tok},
		{method: "POST", path: BasePath + "/cancelServiceOrder", body: `{"serviceOrder":{"id":"` + ids.NewString() + `"}}`, tok: tok},
		{method: "POST", path: BasePath + "/hub", body: `{"callback":"https://localhost:9109/h"}`, tok: tok},
		{method: "DELETE", path: BasePath + "/hub/" + ids.NewString(), tok: tok},
	} {
		rr, m := f.do(t, r)
		if rr.Code != 503 || code(m) != "DXPS-9001" || m["message"] != nil && m["message"] != "" {
			t.Fatal(r.method, r.path, rr.Code, rr.Body)
		}
	}
}

// TC-GW-016: fast-publish failures are logged and left to the outbox relay.
func TestPublishFailure(t *testing.T) {
	g := &Gateway{Pub: failPub{}}
	g.publish(context.Background(), []*store.OutboxRow{{}})
	g.publish(context.Background(), nil)
}

type failPub struct{}

func (failPub) Publish(context.Context, []*store.OutboxRow) error { return io.ErrClosedPipe }
