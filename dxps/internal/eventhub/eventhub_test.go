package eventhub

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"dxps/internal/contract"
	"dxps/internal/secretbox"
	"dxps/internal/store"
	"dxps/internal/testenv/pgenv"
)

func event(tenant, state string) *contract.OrderEvent {
	ev := &contract.OrderEvent{EventID: "ev-1", EventTime: time.Now(), EventType: "ServiceOrderStateChangeEvent", Tenant: tenant}
	ev.Event.ServiceOrder = contract.OrderRef{ID: "o-1", State: state}
	return ev
}

// TC-HUB-001: HMAC-SHA256 signatures verify; tampered body, wrong secret, stale/future timestamps and malformed headers fail.
func TestSignVerify(t *testing.T) {
	secret, body, now := []byte("s3cret"), []byte(`{"a":1}`), time.Now()
	h := Sign(secret, now, body)
	if !strings.HasPrefix(h, "t=") || !strings.Contains(h, ",v1=") {
		t.Fatal(h)
	}
	if err := Verify(secret, h, body, now, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	bad := []struct {
		name, hdr string
		body      []byte
		secret    []byte
		at        time.Time
	}{
		{"tampered", h, []byte(`{"a":2}`), secret, now},
		{"wrong secret", h, body, []byte("other"), now},
		{"replay (stale)", h, body, secret, now.Add(10 * time.Minute)},
		{"future", h, body, secret, now.Add(-10 * time.Minute)},
		{"no v1", "t=1", body, secret, now},
		{"no t", "v1=ab", body, secret, now},
		{"empty", "", body, secret, now},
	}
	for _, c := range bad {
		if err := Verify(c.secret, c.hdr, c.body, c.at, 5*time.Minute); err != ErrSignature {
			t.Fatal(c.name, err)
		}
	}
}

// TC-HUB-002: TMF688 listener query filter on eventType and state (comma lists, unknown keys reject).
func TestMatches(t *testing.T) {
	ev := event("mvno-alpha", "completed")
	cases := map[string]bool{
		"": true, "eventType=ServiceOrderStateChangeEvent": true, "state=completed": true,
		"state=failed,completed": true, "event.serviceOrder.state=completed": true,
		"state=failed": false, "eventType=X": false, "foo=bar": false, "%zz": false,
		"eventType=ServiceOrderStateChangeEvent&state=failed": false,
	}
	for q, want := range cases {
		if Matches(q, ev) != want {
			t.Fatal(q)
		}
	}
}

// TC-HUB-003: callback allow-list: https only, host:port must be allow-listed, no userinfo (SSRF guard).
func TestAllowed(t *testing.T) {
	h := &Hub{Allow: []string{"localhost:9109", "hooks.mvno-alpha.example:443"}}
	cases := map[string]bool{
		"https://localhost:9109/hooks/a":         true,
		"https://LOCALHOST:9109/x":               true,
		"https://hooks.mvno-alpha.example/cb":    true,
		"http://localhost:9109/hooks":            false,
		"https://localhost:9110/":                false,
		"https://169.254.169.254/latest":         false,
		"https://user:pw@localhost:9109/":        false,
		"https://localhost:9109@evil.example/":   false,
		"://bad":                                 false,
		"https://hooks.mvno-alpha.example:8443/": false,
	}
	for cb, want := range cases {
		if h.allowed(cb) != want {
			t.Fatal(cb)
		}
	}
}

type sink struct {
	srv    *httptest.Server
	calls  atomic.Int32
	mu     sync.Mutex
	got    []*http.Request
	bodies [][]byte
}

func newSink(t *testing.T, statuses ...int) *sink {
	s := &sink{}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(s.calls.Add(1))
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.got, s.bodies = append(s.got, r), append(s.bodies, b)
		s.mu.Unlock()
		st := 204
		if n <= len(statuses) {
			st = statuses[n-1]
		}
		if st == 302 {
			w.Header().Set("Location", "https://169.254.169.254/")
		}
		w.WriteHeader(st)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func hubFor(s *sink) *Hub {
	return &Hub{Client: NewClient(s.srv.Client()), Backoff: time.Millisecond, Attempts: 4, Allow: []string{strings.TrimPrefix(s.srv.URL, "https://")}}
}

// TC-HUB-004: delivery signs the body, sets tenant/event headers, retries 5xx/429 with backoff, stops on 4xx and never follows redirects.
func TestDeliver(t *testing.T) {
	ev := event("mvno-alpha", "completed")
	body, _ := json.Marshal(ev)
	secret := []byte("k")
	cases := []struct {
		statuses     []int
		wantStatus   int
		wantAttempts int
		ok           bool
	}{
		{nil, 204, 1, true},
		{[]int{503, 429, 200}, 200, 3, true},
		{[]int{400}, 400, 1, false},
		{[]int{302}, 302, 1, false},
		{[]int{500, 500, 500, 500}, 500, 4, false},
		{[]int{408, 204}, 204, 2, true},
	}
	for _, c := range cases {
		s := newSink(t, c.statuses...)
		st, n, ok := hubFor(s).deliver(context.Background(), s.srv.URL+"/hooks/mvno-alpha", secret, ev, body)
		if st != c.wantStatus || n != c.wantAttempts || ok != c.ok {
			t.Fatalf("%v: got %d/%d/%v", c.statuses, st, n, ok)
		}
		if int(s.calls.Load()) != c.wantAttempts {
			t.Fatalf("%v: server saw %d calls (redirect followed?)", c.statuses, s.calls.Load())
		}
		r := s.got[0]
		if err := Verify(secret, r.Header.Get(SignatureHeader), s.bodies[0], time.Now(), time.Minute); err != nil {
			t.Fatal("signature", err)
		}
		if r.Header.Get("X-DxPS-Tenant") != "mvno-alpha" || r.Header.Get("X-DxPS-Event-Id") != "ev-1" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatal(r.Header)
		}
	}
}

// TC-HUB-005: delivery stops when the context is cancelled and reports transport errors / bad URLs.
func TestDeliverCancelAndErrors(t *testing.T) {
	ev := event("mvno-alpha", "completed")
	s := newSink(t, 503, 503, 503)
	h := hubFor(s)
	h.Backoff = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if st, n, ok := h.deliver(ctx, s.srv.URL, []byte("k"), ev, nil); ok || n != 1 || st != 503 {
		t.Fatal(st, n, ok)
	}
	if _, n, ok := h.deliver(context.Background(), "https://bad host/", []byte("k"), ev, nil); ok || n != 1 {
		t.Fatal(n, ok)
	}
	dead := httptest.NewTLSServer(http.NotFoundHandler())
	dead.Close()
	h2 := &Hub{Client: NewClient(&http.Client{}), Backoff: time.Millisecond, Attempts: 2}
	if st, n, ok := h2.deliver(context.Background(), dead.URL, []byte("k"), ev, nil); ok || n != 2 || st != 0 {
		t.Fatal(st, n, ok)
	}
	h3 := &Hub{Client: NewClient(&http.Client{})}
	if st, n, ok := h3.deliver(context.Background(), dead.URL, []byte("k"), ev, nil); ok || n != 1 || st != 0 {
		t.Fatal("default attempts/backoff", st, n, ok)
	}
	if c := NewClient(&http.Client{Timeout: time.Second}); c.Timeout != time.Second {
		t.Fatal(c.Timeout)
	}
}

// TC-HUB-006: delivery statistics count successes/failures and keep the last 30 deliveries.
func TestStats(t *testing.T) {
	h := &Hub{}
	for i := 0; i < 40; i++ {
		h.note(Delivery{OK: i%4 != 0})
	}
	st := h.Stats()
	if st.Delivered != 30 || st.Failed != 10 || len(st.Recent) != 30 {
		t.Fatalf("%+v", st)
	}
}

// TC-HUB-007: Decode restores the tenant from the Kafka header (never trusted from the payload).
func TestDecode(t *testing.T) {
	ev, err := Decode([]byte(`{"eventId":"e","event":{"serviceOrder":{"id":"o","state":"failed"}}}`), "mvno-beta")
	if err != nil || ev.Tenant != "mvno-beta" || ev.Event.ServiceOrder.State != "failed" {
		t.Fatal(ev, err)
	}
	if _, err := Decode([]byte("{"), "x"); err == nil {
		t.Fatal("decode")
	}
}

// TC-HUB-008 (integration, PostgreSQL): Handle loads the tenant's hubs under RLS, filters by query, decrypts the
// per-tenant secret, refuses non-allow-listed callbacks and rejects tenant mismatches.
func TestHandleIntegration(t *testing.T) {
	st, _ := pgenv.Store(t)
	ctx := context.Background()
	key := make([]byte, 32)
	rand.Read(key)
	box, _ := secretbox.New(key)
	s := newSink(t)
	h := hubFor(s)
	h.Store, h.Box = st, box
	const ten = "zz-test-hub"
	sealed, _ := box.Seal(ten, []byte("tenant-secret"))
	foreign, _ := box.Seal("mvno-alpha", []byte("x"))
	hubs := []*store.Hub{
		{Tenant: ten, Callback: s.srv.URL + "/hooks/" + ten, SecretRef: sealed},
		{Tenant: ten, Callback: s.srv.URL + "/only-failed", Query: "state=failed", SecretRef: sealed},
		{Tenant: ten, Callback: "https://evil.example/steal", SecretRef: sealed},
		{Tenant: ten, Callback: s.srv.URL + "/foreign-secret", SecretRef: foreign},
	}
	err := st.InTenant(ctx, ten, func(tx pgx.Tx) error {
		for _, x := range hubs {
			if err := store.InsertHub(ctx, tx, x); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = st.InTenant(ctx, ten, func(tx pgx.Tx) error {
			for _, x := range hubs {
				_ = store.DeleteHub(ctx, tx, x.ID)
			}
			return nil
		})
	})
	ev := event(ten, "completed")
	if err := h.Handle(ctx, ev, "mvno-alpha", ten+"|o-1"); err != contract.ErrTenantMismatch {
		t.Fatal("tenant mismatch accepted", err)
	}
	if err := h.Handle(ctx, ev, ten, ten+"|o-1"); err != nil {
		t.Fatal(err)
	}
	if s.calls.Load() != 1 || s.got[0].URL.Path != "/hooks/"+ten {
		t.Fatalf("calls=%d", s.calls.Load())
	}
	if err := Verify([]byte("tenant-secret"), s.got[0].Header.Get(SignatureHeader), s.bodies[0], time.Now(), time.Minute); err != nil {
		t.Fatal(err)
	}
	stats := h.Stats()
	if stats.Delivered != 1 || stats.Failed != 2 {
		t.Fatalf("%+v", stats)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := h.Handle(cancelled, ev, ten, ten+"|o-1"); err == nil {
		t.Fatal("cancelled context")
	}
}
