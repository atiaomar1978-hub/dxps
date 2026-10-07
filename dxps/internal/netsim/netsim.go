// Package netsim provides DxPS network element simulators (NRF, UDR, IMS-PG, RESTCONF/USP, SM-DP+,
// OCS/BSS, NPDB, NEF/CAMARA and a TMF688 webhook sink). They speak HTTP/2 over mutual TLS, validate
// payloads, keep per-tenant state and support fault injection and configurable latency.
package netsim

import (
	"bytes"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBody = 256 << 10

// Exchange is one recorded request/response (payload log).
type Exchange struct {
	Sim      string          `json:"sim"`
	At       time.Time       `json:"at"`
	Tenant   string          `json:"tenant"`
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Proto    string          `json:"proto"`
	Status   int             `json:"status"`
	Request  json.RawMessage `json:"request,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
	LatUS    int64           `json:"latUs"`
	Fault    string          `json:"fault,omitempty"`
}

type SimStats struct {
	Name     string        `json:"name"`
	Port     int           `json:"port"`
	Calls    int64         `json:"calls"`
	ByStatus map[int]int64 `json:"byStatus"`
	P50US    float64       `json:"p50us"`
	P99US    float64       `json:"p99us"`
	Faults   int64         `json:"faults"`
	Latency  int           `json:"latencyMs"`
	ErrRate  float64       `json:"errorRate"`
	Objects  int           `json:"objects"`
}

type sim struct {
	name    string
	port    int
	latency time.Duration
	errRate float64
	mux     *http.ServeMux

	mu       sync.Mutex
	calls    int64
	faults   int64
	byStatus map[int]int64
	lats     []float64
	store    map[string]json.RawMessage // tenant|resource -> body
	owner    map[string]string          // resource -> owning tenant (shared NE isolation)
	idem     map[string]Exchange        // idempotency key -> response
	flaky    map[string]int
}

func newSim(name string, port int, latency time.Duration) *sim {
	return &sim{name: name, port: port, latency: latency, mux: http.NewServeMux(), byStatus: map[int]int64{},
		store: map[string]json.RawMessage{}, owner: map[string]string{}, idem: map[string]Exchange{}, flaky: map[string]int{}}
}

type Netsim struct {
	sims    []*sim
	byName  map[string]*sim
	tokKey  []byte
	mu      sync.Mutex
	log     []Exchange
	Host    string
	admin   string
	servers []*http.Server
}

// New creates all simulators. adminToken protects the admin API.
func New(host, adminToken string) *Netsim {
	k := make([]byte, 32)
	_, _ = crand.Read(k)
	n := &Netsim{byName: map[string]*sim{}, tokKey: k, Host: host, admin: adminToken}
	add := func(s *sim) *sim {
		n.sims = append(n.sims, s)
		n.byName[s.name] = s
		return s
	}
	n.nrf(add(newSim("nrf", 9101, 1*time.Millisecond)))
	n.udr(add(newSim("udr", 9102, 2*time.Millisecond)))
	n.ims(add(newSim("ims-pg", 9103, 3*time.Millisecond)))
	n.restconf(add(newSim("restconf", 9104, 12*time.Millisecond)))
	n.smdp(add(newSim("smdp", 9105, 20*time.Millisecond)))
	n.ocs(add(newSim("ocs-bss", 9106, 4*time.Millisecond)))
	n.npdb(add(newSim("npdb", 9107, 5*time.Millisecond)))
	n.nef(add(newSim("nef", 9108, 6*time.Millisecond)))
	n.sink(add(newSim("hub-sink", 9109, 0)))
	return n
}

// Handler returns the handler for a simulator (tests use it with httptest).
func (n *Netsim) Handler(name string) http.Handler {
	s := n.byName[name]
	return n.wrap(s)
}

func (n *Netsim) Ports() map[string]int {
	out := map[string]int{}
	for _, s := range n.sims {
		out[s.name] = s.port
	}
	return out
}

// Serve starts all simulators. serverTLS should require client certificates for NE simulators.
func (n *Netsim) Serve(neTLS, sinkTLS *tls.Config) error {
	for _, s := range n.sims {
		cfg := neTLS
		if s.name == "hub-sink" {
			cfg = sinkTLS
		}
		ln, err := tls.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(s.port)), cfg)
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: n.wrap(s), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
			WriteTimeout: 15 * time.Second, IdleTimeout: 90 * time.Second}
		n.servers = append(n.servers, srv)
		go srv.Serve(ln)
	}
	return nil
}

func (n *Netsim) Close() {
	for _, s := range n.servers {
		s.Close()
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// ---------------------------------------------------------------- recording / faults middleware

type recorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (r *recorder) WriteHeader(s int) { r.status = s; r.ResponseWriter.WriteHeader(s) }
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	if r.buf.Len() < 4096 {
		r.buf.Write(b)
	}
	return r.ResponseWriter.Write(b)
}

var numRe = regexp.MustCompile(`[0-9]{8,32}`)

// faultFor returns the injected fault for a request (by key suffix) or "".
func faultFor(path string, body []byte) string {
	for _, m := range numRe.FindAllString(path+" "+string(body), -1) {
		switch {
		case strings.HasSuffix(m, "999"):
			return "unavailable"
		case strings.HasSuffix(m, "998"):
			return "invalid"
		case strings.HasSuffix(m, "997"):
			return "flaky"
		}
	}
	return ""
}

// sensitiveKeys are masked in recorded exchanges: the exchange log is shown on the dashboard and exported to
// test reports, so it must never carry credentials.
var sensitiveKeys = map[string]bool{"access_token": true, "refresh_token": true, "id_token": true, "client_secret": true,
	"password": true, "secret": true, "authorization": true}

const redacted = "***redacted***"

func redact(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if sensitiveKeys[strings.ToLower(k)] {
				x[k] = redacted
			} else {
				x[k] = redact(e)
			}
		}
	case []any:
		for i, e := range x {
			x[i] = redact(e)
		}
	}
	return v
}

func compactJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		if q, err := url.ParseQuery(string(b)); err == nil {
			hit := false
			for k := range q {
				if sensitiveKeys[strings.ToLower(k)] {
					q[k], hit = []string{redacted}, true
				}
			}
			if hit {
				b = []byte(q.Encode())
			}
		}
		s, _ := json.Marshal(truncate(string(b), 512))
		return s
	}
	b, _ = json.Marshal(redact(v))
	var out bytes.Buffer
	_ = json.Compact(&out, b)
	if out.Len() > 2048 {
		s, _ := json.Marshal(truncate(out.String(), 2048))
		return s
	}
	return out.Bytes()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func (n *Netsim) wrap(s *sim) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil || len(body) > maxBody {
			problem(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "body too large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		s.mu.Lock()
		lat, er := s.latency, s.errRate
		s.mu.Unlock()
		if lat > 0 {
			time.Sleep(lat + time.Duration(rand.Int64N(int64(lat)/2+1)))
		}
		rec := &recorder{ResponseWriter: w}
		fault := ""
		if s.name != "hub-sink" && s.name != "nrf" {
			fault = faultFor(r.URL.Path, body)
			if fault == "" && er > 0 && rand.Float64() < er {
				fault = "random"
			}
		}
		switch fault {
		case "unavailable", "random":
			rec.Header().Set("Retry-After", "1")
			problem(rec, http.StatusServiceUnavailable, "NF_CONGESTION", "simulated overload")
		case "invalid":
			if s.name == "smdp" { // ES2+ reports business errors inside a 200 response
				fault = ""
				s.mux.ServeHTTP(rec, r)
			} else {
				problem(rec, http.StatusBadRequest, "MANDATORY_IE_INCORRECT", "simulated validation error")
			}
		case "flaky":
			key := r.Method + " " + r.URL.Path
			s.mu.Lock()
			s.flaky[key]++
			c := s.flaky[key]
			s.mu.Unlock()
			if c <= 2 {
				rec.Header().Set("Retry-After", "0")
				problem(rec, http.StatusServiceUnavailable, "TEMPORARILY_UNAVAILABLE", "simulated transient failure")
			} else {
				fault = "flaky-recovered"
				s.mux.ServeHTTP(rec, r)
			}
		default:
			s.mux.ServeHTTP(rec, r)
		}
		if rec.status == 0 {
			rec.status = 200
		}
		el := time.Since(start)
		s.mu.Lock()
		s.calls++
		s.byStatus[rec.status]++
		if fault != "" && fault != "flaky-recovered" {
			s.faults++
		}
		s.lats = append(s.lats, float64(el.Microseconds()))
		if len(s.lats) > 2000 {
			s.lats = s.lats[len(s.lats)-2000:]
		}
		s.mu.Unlock()
		n.mu.Lock()
		n.log = append(n.log, Exchange{Sim: s.name, At: start, Tenant: r.Header.Get("X-DxPS-Tenant"), Method: r.Method,
			Path: r.URL.Path, Proto: r.Proto, Status: rec.status, Request: compactJSON(body), Response: compactJSON(rec.buf.Bytes()),
			LatUS: el.Microseconds(), Fault: fault})
		if len(n.log) > 300 {
			n.log = n.log[len(n.log)-300:]
		}
		n.mu.Unlock()
	})
}

func problem(w http.ResponseWriter, status int, cause, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "cause": cause, "detail": detail, "title": http.StatusText(status)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------- state helpers

func (s *sim) put(tenant, key string, v json.RawMessage) (created bool, conflict bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.owner[key]; ok && o != tenant {
		return false, true
	}
	_, exists := s.store[tenant+"|"+key]
	s.store[tenant+"|"+key] = append(json.RawMessage(nil), v...)
	s.owner[key] = tenant
	return !exists, false
}

func (s *sim) get(tenant, key string) (json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.store[tenant+"|"+key]
	return v, ok
}

func (s *sim) del(tenant, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.owner[key]; ok && o != tenant {
		return false
	}
	delete(s.store, tenant+"|"+key)
	delete(s.owner, key)
	return true
}

// replay returns a stored response for an idempotency key.
func (s *sim) replay(key string) (Exchange, bool) {
	if key == "" {
		return Exchange{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.idem[key]
	return e, ok
}

func (s *sim) remember(key string, status int, body any) {
	if key == "" {
		return
	}
	b, _ := json.Marshal(body)
	s.mu.Lock()
	s.idem[key] = Exchange{Status: status, Response: b}
	s.mu.Unlock()
}

func tenantOf(r *http.Request) string {
	if t := r.Header.Get("X-DxPS-Tenant"); t != "" {
		return t
	}
	return "unknown"
}

// ---------------------------------------------------------------- admin API (loopback)

func (n *Netsim) Stats() []SimStats {
	var out []SimStats
	for _, s := range n.sims {
		s.mu.Lock()
		st := SimStats{Name: s.name, Port: s.port, Calls: s.calls, ByStatus: map[int]int64{}, Faults: s.faults,
			Latency: int(s.latency / time.Millisecond), ErrRate: s.errRate, Objects: len(s.store)}
		for k, v := range s.byStatus {
			st.ByStatus[k] = v
		}
		l := append([]float64(nil), s.lats...)
		s.mu.Unlock()
		st.P50US, st.P99US = pct(l, 0.5), pct(l, 0.99)
		out = append(out, st)
	}
	return out
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	i := int(math.Ceil(p*float64(len(xs)))) - 1
	return xs[max(0, min(i, len(xs)-1))]
}

func (n *Netsim) Exchanges(limit int) []Exchange {
	n.mu.Lock()
	defer n.mu.Unlock()
	if limit <= 0 || limit > len(n.log) {
		limit = len(n.log)
	}
	out := make([]Exchange, limit)
	copy(out, n.log[len(n.log)-limit:])
	return out
}

// AdminToken derives the admin token from a secret (shared with the dashboard).
func AdminToken(secret []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("dxps-netsim-admin"))
	return hex.EncodeToString(m.Sum(nil))[:40]
}

type FaultReq struct {
	Sim       string  `json:"sim"`
	LatencyMs int     `json:"latencyMs"`
	ErrorRate float64 `json:"errorRate"`
}

// AdminHandler serves /stats, /exchanges and /faults. It must be bound to loopback.
func (n *Netsim) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, n.Stats()) })
	mux.HandleFunc("GET /exchanges", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, n.Exchanges(60)) })
	mux.HandleFunc("POST /faults", func(w http.ResponseWriter, r *http.Request) {
		var f FaultReq
		dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil || f.LatencyMs < 0 || f.LatencyMs > 5000 || f.ErrorRate < 0 || f.ErrorRate > 1 {
			problem(w, 400, "INVALID", "invalid fault request")
			return
		}
		s := n.byName[f.Sim]
		if s == nil {
			problem(w, 404, "NOT_FOUND", "unknown simulator")
			return
		}
		s.mu.Lock()
		s.latency, s.errRate = time.Duration(f.LatencyMs)*time.Millisecond, f.ErrorRate
		s.mu.Unlock()
		writeJSON(w, 200, f)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hmac.Equal([]byte(r.Header.Get("X-Netsim-Admin")), []byte(n.admin)) || n.admin == "" {
			problem(w, 401, "UNAUTHORIZED", "admin token required")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

var errAdminBind = errors.New("admin must bind to loopback")

func ServeAdmin(addr string, h http.Handler) (*http.Server, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || (host != "127.0.0.1" && host != "localhost") {
		return nil, errAdminBind
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 3 * time.Second}
	go srv.Serve(ln)
	return srv, nil
}
