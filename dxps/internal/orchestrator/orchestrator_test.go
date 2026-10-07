package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"dxps/internal/adapter"
	"dxps/internal/auth"
	"dxps/internal/contract"
	"dxps/internal/gateway"
	"dxps/internal/ids"
	"dxps/internal/netsim"
	"dxps/internal/secretbox"
	"dxps/internal/store"
	"dxps/internal/testenv"
	"dxps/internal/testenv/pgenv"
)

// ---------------------------------------------------------------- unit

// TC-ORC-001: DXPS error codes are extracted from wrapped errors; default otherwise.
func TestErrCode(t *testing.T) {
	if errCode(errors.New("DXPS-1004: tenant suspended"), "DXPS-1002") != "DXPS-1004" || errCode(errors.New("boom"), "DXPS-1002") != "DXPS-1002" {
		t.Fatal("errCode")
	}
	if codeOf(nil) != "DXPS-3000" || codeOf(&store.Task{NECode: "DXPS-3409"}) != "DXPS-3409" {
		t.Fatal("codeOf")
	}
}

// TC-ORC-002: transient errors are retried with backoff; permanent errors and cancelled contexts stop at once.
func TestWithRetry(t *testing.T) {
	n := 0
	if err := withRetry(context.Background(), func() error { n++; return map[bool]error{true: errors.New("transient")}[n < 3] }); err != nil || n != 3 {
		t.Fatal(err, n)
	}
	n = 0
	if err := withRetry(context.Background(), func() error { n++; return fmt.Errorf("%w: bad", ErrPermanent) }); !errors.Is(err, ErrPermanent) || n != 1 {
		t.Fatal(err, n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n = 0
	if err := withRetry(ctx, func() error { n++; return errors.New("x") }); err == nil || n != 1 {
		t.Fatal(err, n)
	}
}

// TC-ORC-003: the stats endpoint refuses non-loopback binds.
func TestServeStatsLoopback(t *testing.T) {
	for _, a := range []string{"0.0.0.0:9201", ":9201", "bad"} {
		if err := serveStats(context.Background(), a, nil, nil); err == nil {
			t.Fatal(a)
		}
	}
	o := &Orchestrator{}
	if o.log() == nil || time.Since(o.now()) > time.Second {
		t.Fatal("defaults")
	}
}

// ---------------------------------------------------------------- integration harness

// pump stands in for Kafka: it captures outbox rows and delivers them synchronously.
type pump struct {
	mu     sync.Mutex
	st     *store.Store
	queue  []*store.OutboxRow
	events map[string][]string // order -> TMF688 states
	dlq    []map[string]any
	retry  int
}

func (p *pump) Publish(ctx context.Context, rows []*store.OutboxRow) error {
	p.mu.Lock()
	p.queue = append(p.queue, rows...)
	p.mu.Unlock()
	return p.st.MarkPublished(ctx, rows) // keep the live outbox relay away from test rows
}

func (p *pump) pop() *store.OutboxRow {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return nil
	}
	r := p.queue[0]
	p.queue = p.queue[1:]
	return r
}

type harness struct {
	o  *Orchestrator
	a  *adapter.Adapter
	gw http.Handler
	p  *pump
	ns *netsim.Netsim
	tk func(tenant string) string

	lastReq, lastResp json.RawMessage
}

// capture writes the TMF641 request/response, the final order, TMF688 states and the NE exchanges of a test
// run to $DXPS_CAPTURE_DIR/<name>.json (used for the test-case document payload appendix).
func (h *harness) capture(t *testing.T, name, tenantID, id string) {
	dir := os.Getenv("DXPS_CAPTURE_DIR")
	if dir == "" {
		return
	}
	r := httptest.NewRequest("GET", gateway.BasePath+"/serviceOrder/"+id, nil)
	r.Header.Set("Authorization", "Bearer "+h.tk(tenantID))
	rr := httptest.NewRecorder()
	h.gw.ServeHTTP(rr, r)
	art := map[string]any{"test": t.Name(), "tenant": tenantID, "tmf641Request": h.lastReq, "tmf641Response": h.lastResp,
		"tmf641Get": json.RawMessage(rr.Body.Bytes()), "tmf688States": h.p.events[id], "neExchanges": h.ns.Exchanges(200)}
	b, err := json.MarshalIndent(art, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, name+".json"), b, 0o600)
	}
	if err != nil {
		t.Log("capture:", err)
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, _ := pgenv.Store(t)
	ctx := context.Background()
	o, err := New(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &pump{st: st, events: map[string][]string{}}
	o.Pub = p
	// Network simulators behind one mTLS server; the DB registry is re-pointed at it.
	ns := netsim.New("localhost", "x")
	adm := ns.AdminHandler()
	for name := range ns.Ports() {
		r := httptest.NewRequest("POST", "/faults", strings.NewReader(`{"sim":"`+name+`","latencyMs":0}`))
		r.Header.Set("X-Netsim-Admin", "x")
		adm.ServeHTTP(httptest.NewRecorder(), r)
	}
	mux := http.NewServeMux()
	for prefix, sim := range map[string]string{"/oauth2/": "nrf", "/nudr-dr/": "udr", "/ims-pg/": "ims-pg", "/restconf/": "restconf",
		"/usp/": "restconf", "/gsma/": "smdp", "/tmf-api/": "ocs-bss", "/npdb/": "npdb", "/quality-on-demand/": "nef"} {
		mux.Handle(prefix, ns.Handler(sim))
	}
	pk := testenv.NewPKI(t)
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = pk.ServerTLS(t, true)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	for _, ne := range o.Registry.All() {
		for i := range ne.Endpoints {
			ne.Endpoints[i].BaseURI = srv.URL
		}
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: pk.ClientTLS(t, true), ForceAttemptHTTP2: true}}
	sig, _ := auth.NewSigner([]byte("0123456789abcdef0123456789abcdef"), "dxps-test", "dxps-api")
	key := make([]byte, 32)
	rand.Read(key)
	box, _ := secretbox.New(key)
	g := &gateway.Gateway{Store: st, Catalog: o.Catalog, Tenants: o.Tenants, Signer: sig, Box: box, Pub: p}
	return &harness{o: o, a: adapter.New(o.Registry, client), gw: g.Handler(), p: p, ns: ns, tk: func(ten string) string {
		tok, _ := sig.Issue("orc-test", ten, []string{auth.ScopeOrderWrite, auth.ScopeOrderRead}, time.Minute)
		return tok
	}}
}

// submit posts a TMF641 order through the real gateway and returns its id.
func (h *harness) submit(t *testing.T, ten string, items ...string) string {
	t.Helper()
	body := `{"serviceOrderItem":[` + strings.Join(items, ",") + `]}`
	r := httptest.NewRequest("POST", gateway.BasePath+"/serviceOrder", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+h.tk(ten))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "orc-"+ids.NewString())
	rr := httptest.NewRecorder()
	h.gw.ServeHTTP(rr, r)
	if rr.Code != 201 {
		t.Fatal(rr.Code, rr.Body)
	}
	h.lastReq, h.lastResp = json.RawMessage(body), json.RawMessage(rr.Body.Bytes())
	var m map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	return m["id"].(string)
}

// run drains the pump: BCs -> orchestrator, tasks -> adapter (network simulators) -> results -> orchestrator.
func (h *harness) run(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 2000; i++ {
		r := h.p.pop()
		if r == nil {
			return
		}
		switch {
		case strings.HasPrefix(r.Topic, "dxps.bc."):
			var bc contract.BusinessCommand
			_ = json.Unmarshal(r.Payload, &bc)
			if err := h.o.HandleBC(ctx, &bc, r.Headers[contract.HdrTenant], r.Key); err != nil {
				t.Fatal("HandleBC", err)
			}
		case strings.HasPrefix(r.Topic, "dxps.task.") || strings.HasPrefix(r.Topic, "dxps.retry."):
			if strings.HasPrefix(r.Topic, "dxps.retry.") {
				h.p.retry++
				if r.Headers[contract.HdrTarget] == "" || r.Headers[contract.HdrNotBefore] == "" {
					t.Fatal("retry row without target/not_before headers")
				}
			}
			var nt contract.NeTask
			_ = json.Unmarshal(r.Payload, &nt)
			res := h.a.Execute(ctx, &nt)
			if err := h.o.HandleResult(ctx, res, res.Tenant, contract.Key(res.Tenant, res.OrderID)); err != nil {
				t.Fatal("HandleResult", err)
			}
		case r.Topic == contract.TopicEvent:
			var ev contract.OrderEvent
			_ = json.Unmarshal(r.Payload, &ev)
			h.p.events[ev.Event.ServiceOrder.ID] = append(h.p.events[ev.Event.ServiceOrder.ID], ev.Event.ServiceOrder.State)
		case r.Topic == contract.TopicDLQ:
			var m map[string]any
			_ = json.Unmarshal(r.Payload, &m)
			h.p.dlq = append(h.p.dlq, m)
		}
	}
	t.Fatal("pump did not drain")
}

type orderResult struct {
	state string
	tasks []*store.Task
}

func (h *harness) result(t *testing.T, ten, id string) orderResult {
	t.Helper()
	var out orderResult
	err := h.o.Store.InTenant(context.Background(), ten, func(tx pgx.Tx) error {
		o, err := store.GetOrder(context.Background(), tx, id)
		if err != nil {
			return err
		}
		out.state = o.State
		bcs, err := store.OrderBCs(context.Background(), tx, ten, id)
		if err != nil {
			return err
		}
		for _, b := range bcs {
			ts, _, err := store.TasksOfBC(context.Background(), tx, ten, b.ID)
			if err != nil {
				return err
			}
			out.tasks = append(out.tasks, ts...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (r orderResult) count(state string, comp bool) int {
	n := 0
	for _, t := range r.tasks {
		if t.State == state && t.IsCompensation == comp {
			n++
		}
	}
	return n
}

var seq = time.Now().UnixNano() % 100_000_000

func sub(spec string, id string, msisdnSuffix string, deps ...string) (string, string, string) {
	seq++
	supi := fmt.Sprintf("imsi-4167755%08d", seq)
	msisdn := fmt.Sprintf("1416%08d", seq)
	if msisdnSuffix != "" {
		msisdn = msisdn[:len(msisdn)-3] + msisdnSuffix
	}
	return itemJSON(id, spec, map[string]string{"supi": supi, "msisdn": msisdn, "plan": "5G-100GB"}, deps...), supi, msisdn
}

func itemJSON(id, spec string, params map[string]string, deps ...string) string {
	var cs []string
	for k, v := range params {
		cs = append(cs, fmt.Sprintf(`{"name":%q,"value":%q}`, k, v))
	}
	rel := ""
	for _, d := range deps {
		rel = fmt.Sprintf(`,"serviceOrderItemRelationship":[{"relationshipType":"dependsOn","orderItem":{"itemId":%q}}]`, d)
	}
	return fmt.Sprintf(`{"id":%q,"action":"add","service":{"serviceSpecification":{"id":%q},"serviceCharacteristic":[%s]}%s}`, id, spec, strings.Join(cs, ","), rel)
}

const ten = "mvno-beta"

// TC-ORC-004 (E2E): CreateSubscriber5G runs its DAG (UDR auth -> am/smf/policy -> OCS) to completion; TMF688 events
// report acknowledged -> inProgress -> completed.
func TestCreateSubscriberE2E(t *testing.T) {
	h := newHarness(t)
	it, _, _ := sub("CreateSubscriber5G", "1", "")
	id := h.submit(t, ten, it)
	h.run(t)
	r := h.result(t, ten, id)
	if r.state != "completed" || r.count(store.TaskSucceeded, false) != 5 || r.count(store.TaskSucceeded, true) != 0 {
		t.Fatalf("state=%s tasks=%d", r.state, len(r.tasks))
	}
	ev := h.p.events[id]
	if len(ev) < 2 || ev[0] != "inProgress" || ev[len(ev)-1] != "completed" {
		t.Fatal(ev)
	}
	h.capture(t, "e2e-create-subscriber-5g", ten, id)
}

// TC-ORC-005 (E2E saga): a permanent NE rejection (998) in an ATOMIC command compensates every succeeded task in
// reverse order, fails the order and writes an audit record to the DLQ.
func TestCompensationE2E(t *testing.T) {
	h := newHarness(t)
	it, _, _ := sub("CreateSubscriber5G", "1", "998")
	id := h.submit(t, ten, it)
	h.run(t)
	r := h.result(t, ten, id)
	if r.state != "failed" || r.count(store.TaskFailed, false) != 1 || r.count(store.TaskSucceeded, true) == 0 {
		for _, x := range r.tasks {
			t.Logf("%s %s comp=%v %s", x.Operation, x.State, x.IsCompensation, x.NECode)
		}
		t.Fatalf("state=%s", r.state)
	}
	for _, x := range r.tasks {
		if !x.IsCompensation && x.State == store.TaskSucceeded {
			t.Fatalf("forward task %s succeeded but was not compensated", x.Operation)
		}
	}
	if len(h.p.dlq) == 0 || h.p.dlq[0]["code"] == "" {
		t.Fatal("no DLQ audit record", h.p.dlq)
	}
	if ev := h.p.events[id]; ev[len(ev)-1] != "failed" {
		t.Fatal(ev)
	}
	h.capture(t, "e2e-saga-compensation", ten, id)
}

// TC-ORC-006 (E2E retry ladder): transient 503s (997) are retried through the retry topics and the order completes.
func TestRetryLadderE2E(t *testing.T) {
	h := newHarness(t)
	it, _, _ := sub("CreateSubscriber5G", "1", "997")
	id := h.submit(t, ten, it)
	h.run(t)
	if r := h.result(t, ten, id); r.state != "completed" {
		t.Fatal(r.state)
	}
	if h.p.retry < 2 {
		t.Fatal("expected retries via the retry ladder, got", h.p.retry)
	}
	h.capture(t, "e2e-retry-ladder", ten, id)
}

// TC-ORC-007 (E2E): retries are bounded: a permanently unavailable NE (999) exhausts MaxAttempts -> DXPS-4001, rollback.
func TestRetryExhaustedE2E(t *testing.T) {
	h := newHarness(t)
	h.o.MaxAttempts = 3
	it, _, _ := sub("CreateSubscriber5G", "1", "999")
	id := h.submit(t, ten, it)
	h.run(t)
	r := h.result(t, ten, id)
	var code string
	for _, x := range r.tasks {
		if x.State == store.TaskFailed {
			code = x.NECode
		}
	}
	if r.state != "failed" || code != "DXPS-4001" {
		t.Fatal(r.state, code)
	}
}

// TC-ORC-008 (E2E): order item dependsOn: AddVoNR waits for CreateSubscriber5G, then runs IMS IMPI -> IMPU -> MMTEL.
func TestItemDependencyE2E(t *testing.T) {
	h := newHarness(t)
	it, supi, msisdn := sub("CreateSubscriber5G", "sub", "")
	vonr := itemJSON("volte", "AddVoNR", map[string]string{"supi": supi, "msisdn": msisdn}, "sub")
	id := h.submit(t, ten, it, vonr)
	h.run(t)
	r := h.result(t, ten, id)
	ims := 0
	for _, x := range r.tasks {
		if x.Domain == "ims" && x.State == store.TaskSucceeded {
			ims++
		}
	}
	if r.state != "completed" || ims < 3 {
		t.Fatal(r.state, ims)
	}
	h.capture(t, "e2e-dependson-vonr", ten, id)
}

// TC-ORC-008b (E2E): a failed prerequisite item fails the dependent item without calling its NEs.
func TestItemDependencyFailedE2E(t *testing.T) {
	h := newHarness(t)
	it, supi, msisdn := sub("CreateSubscriber5G", "sub", "998")
	vonr := itemJSON("volte", "AddVoNR", map[string]string{"supi": supi, "msisdn": msisdn}, "sub")
	id := h.submit(t, ten, it, vonr)
	h.run(t)
	r := h.result(t, ten, id)
	for _, x := range r.tasks {
		if x.Domain == "ims" {
			t.Fatal("dependent item must not run", x.Operation)
		}
	}
	if r.state != "failed" {
		t.Fatal(r.state)
	}
}

// TC-ORC-009 (E2E entity sequencer): two orders on the same subscriber are serialised: the second is parked until
// the first finishes, then admitted automatically.
func TestEntitySequencerE2E(t *testing.T) {
	h := newHarness(t)
	it, supi, msisdn := sub("CreateSubscriber5G", "1", "")
	first := h.submit(t, ten, it)
	second := h.submit(t, ten, itemJSON("1", "ChangePlan", map[string]string{"supi": supi, "msisdn": msisdn, "plan": "5G-UNLIMITED"}))
	// Deliver both BCs before any task runs so the second one finds the entity busy.
	var bcs, rest []*store.OutboxRow
	for r := h.p.pop(); r != nil; r = h.p.pop() {
		if strings.HasPrefix(r.Topic, "dxps.bc.") {
			bcs = append(bcs, r)
		} else {
			rest = append(rest, r)
		}
	}
	for _, r := range bcs {
		var bc contract.BusinessCommand
		_ = json.Unmarshal(r.Payload, &bc)
		if err := h.o.HandleBC(context.Background(), &bc, r.Headers[contract.HdrTenant], r.Key); err != nil {
			t.Fatal(err)
		}
	}
	if s := h.result(t, ten, second); s.state != "pending" || len(s.tasks) != 0 {
		t.Fatal("second order must be parked:", s.state, len(s.tasks))
	}
	h.p.mu.Lock()
	h.p.queue = append(rest, h.p.queue...)
	h.p.mu.Unlock()
	h.run(t)
	if a, b := h.result(t, ten, first), h.result(t, ten, second); a.state != "completed" || b.state != "completed" {
		t.Fatal(a.state, b.state)
	}
}

// TC-ORC-010: BCs for unknown specs or suspended tenants are rejected (order state rejected); duplicates are ignored;
// tenant header/key mismatches and invalid messages are permanent errors (DLQ).
func TestHandleBCGuards(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mk := func(tenant, spec string) *contract.BusinessCommand {
		oid := ids.NewString()
		return &contract.BusinessCommand{Tenant: tenant, MessageID: ids.NewString(), OrderID: oid, OrderItemID: "1", BCID: ids.NewString(),
			EntityKey: "supi:imsi-416770000000001", CommandSpec: spec, Action: "add", Priority: contract.P2, TxMode: contract.Atomic,
			Params: map[string]any{"supi": "imsi-416770000000001", "msisdn": "14165550000", "plan": "5G-100GB"}}
	}
	bc := mk(ten, "NoSuchSpec")
	if err := h.o.HandleBC(ctx, bc, ten, ten+"|k"); err != nil {
		t.Fatal(err)
	}
	if r := h.result(t, ten, bc.OrderID); r.state != "rejected" {
		t.Fatal("unknown spec:", r.state)
	}
	if err := h.o.HandleBC(ctx, bc, ten, ten+"|k"); err != nil { // duplicate message id: no-op
		t.Fatal(err)
	}
	gamma := mk("mvno-gamma", "CreateSubscriber5G")
	if err := h.o.HandleBC(ctx, gamma, "mvno-gamma", "mvno-gamma|k"); err != nil {
		t.Fatal(err)
	}
	if r := h.result(t, "mvno-gamma", gamma.OrderID); r.state != "rejected" {
		t.Fatal("suspended tenant:", r.state)
	}
	if err := h.o.HandleBC(ctx, mk(ten, "CreateSubscriber5G"), "mvno-alpha", ten+"|k"); !errors.Is(err, ErrPermanent) {
		t.Fatal("tenant header mismatch", err)
	}
	if err := h.o.HandleBC(ctx, &contract.BusinessCommand{Tenant: ten}, ten, ten+"|k"); !errors.Is(err, ErrPermanent) {
		t.Fatal("invalid BC", err)
	}
}

// TC-ORC-011: TaskResult guards: tenant mismatch / invalid ids are permanent; unknown orders, stale attempts and
// duplicate results are ignored.
func TestHandleResultGuards(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.o.HandleResult(ctx, &contract.TaskResult{Tenant: ten, TaskID: "x", OrderID: "y"}, ten, ten+"|y"); !errors.Is(err, ErrPermanent) {
		t.Fatal(err)
	}
	if err := h.o.HandleResult(ctx, &contract.TaskResult{Tenant: ten}, "mvno-alpha", ten+"|y"); !errors.Is(err, ErrPermanent) {
		t.Fatal(err)
	}
	if err := h.o.HandleResult(ctx, &contract.TaskResult{Tenant: ten, TaskID: ids.NewString(), OrderID: ids.NewString(), Outcome: contract.Succeeded}, ten, ten+"|y"); err != nil {
		t.Fatal("unknown order", err)
	}
	it, _, _ := sub("CreateSubscriber5G", "1", "")
	id := h.submit(t, ten, it)
	r := h.p.pop()
	var bc contract.BusinessCommand
	_ = json.Unmarshal(r.Payload, &bc)
	if err := h.o.HandleBC(ctx, &bc, ten, r.Key); err != nil {
		t.Fatal(err)
	}
	var task contract.NeTask
	for x := h.p.pop(); x != nil; x = h.p.pop() {
		if strings.HasPrefix(x.Topic, "dxps.task.") {
			_ = json.Unmarshal(x.Payload, &task)
		}
	}
	stale := &contract.TaskResult{Tenant: ten, TaskID: task.TaskID, OrderID: id, Attempt: 5, Outcome: contract.Succeeded}
	if err := h.o.HandleResult(ctx, stale, ten, ten+"|"+id); err != nil {
		t.Fatal(err)
	}
	if err := h.o.HandleResult(ctx, &contract.TaskResult{Tenant: ten, TaskID: ids.NewString(), OrderID: id, Outcome: contract.Succeeded}, ten, ten+"|"+id); err != nil {
		t.Fatal("unknown task", err)
	}
	if got := h.result(t, ten, id); got.count(store.TaskReady, false) != 1 {
		t.Fatal("stale result must not change the task")
	}
}

// TC-ORC-012 (Kafka + PostgreSQL): the service wiring starts the tiered BC/result consumers, outbox relay,
// retry forwarder, registry refresh and the loopback stats endpoint, then shuts down cleanly on cancel.
func TestServiceRun(t *testing.T) {
	st, cfg := pgenv.Store(t)
	testenv.Kafka(t)
	o, err := New(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- o.Run(ctx, ServiceConfig{Brokers: cfg.Kafka, Instance: "test-" + rand.Text()[:8], StatsAddr: addr})
	}()
	var stats map[string]json.RawMessage
	for deadline := time.Now().Add(20 * time.Second); ; {
		if resp, err := http.Get("http://" + addr + "/stats"); err == nil {
			err = json.NewDecoder(resp.Body).Decode(&stats)
			resp.Body.Close()
			if err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("stats endpoint never came up")
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, k := range []string{"bcScheduler", "bcLanes", "resultScheduler", "resultLanes"} {
		if _, ok := stats[k]; !ok {
			t.Fatal("stats missing", k)
		}
	}
	if o.Pub == nil {
		t.Fatal("publisher not wired")
	}
	time.Sleep(1200 * time.Millisecond) // at least two relay ticks
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
	if err := (&Orchestrator{Store: st, Tenants: o.Tenants, Registry: o.Registry}).Run(context.Background(), ServiceConfig{}); err == nil {
		t.Fatal("Run without brokers must fail")
	}
}
