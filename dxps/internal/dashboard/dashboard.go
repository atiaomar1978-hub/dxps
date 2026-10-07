// Package dashboard serves the DxPS operations mosaic: a live, tile-based view of tenants, priority
// lanes, Kafka topics, orders, NE health, payload exchanges, webhooks, security probes and tests.
package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"dxps/internal/auth"
	"dxps/internal/bus"
	"dxps/internal/catalog"
	"dxps/internal/store"
	"dxps/internal/tenant"
)

//go:embed static
var static embed.FS

// Sources are the loopback endpoints the snapshot aggregates.
type Sources struct {
	Orchestrator string // http://127.0.0.1:9201/stats
	Adapter      string
	EventHub     string
	NetsimAdmin  string // http://127.0.0.1:9199
	NetsimToken  string
	Results      string // tests.json written by the test runner
	NEProbe      string // mTLS NE URL used by the SEC-23/24 probes (default: netsim UDR)
}

type Server struct {
	Store    *store.Store
	Admin    *bus.Admin
	Catalog  *catalog.Catalog
	Signer   *auth.Signer
	JWTKey   []byte // for the expired / wrong-audience security probes
	Issuer   string
	Audience string
	Src      Sources
	Gateway  *http.Client // trusts the dev CA
	GWURL    string
	Origins  []string
	Log      *slog.Logger

	csrfKey []byte
	local   *http.Client
	demo    *Demo

	mu     sync.Mutex
	cached []byte
	at     time.Time
	hist   []Point
}

// Point is one sample of the throughput history sparkline.
type Point struct {
	T      int64   `json:"t"`
	Orders int64   `json:"orders"`
	Tasks  int64   `json:"tasks"`
	P99    float64 `json:"p99"`
	Lag    int64   `json:"lag"`
}

func New(s *Server) *Server {
	s.csrfKey = make([]byte, 32)
	_, _ = rand.Read(s.csrfKey)
	s.local = &http.Client{Timeout: 1500 * time.Millisecond}
	s.demo = &Demo{s: s, Probes: []ProbeResult{}}
	if s.Log == nil {
		s.Log = slog.Default()
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("GET /api/snapshot", s.snapshot)
	mux.HandleFunc("POST /api/demo", s.mutating(s.demo.start))
	mux.HandleFunc("POST /api/probe", s.mutating(s.demo.probe))
	mux.HandleFunc("POST /api/fault", s.mutating(s.fault))
	return s.hostOnly(headers(mux))
}

// hostOnly rejects requests whose Host is not one of the configured origins (DNS-rebinding guard:
// a hostile page re-resolved to 127.0.0.1 still sends its own Host header).
func (s *Server) hostOnly(next http.Handler) http.Handler {
	hosts := map[string]bool{}
	for _, o := range s.Origins {
		if _, h, ok := strings.Cut(o, "://"); ok {
			hosts[strings.ToLower(h)] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[strings.ToLower(r.Host)] {
			http.Error(w, "host not allowed", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- CSRF (double submit, HMAC bound)

func (s *Server) csrfToken(nonce string) string {
	m := hmac.New(sha256.New, s.csrfKey)
	m.Write([]byte(nonce))
	return nonce + "." + hex.EncodeToString(m.Sum(nil))
}

func (s *Server) csrfValid(tok string) bool {
	nonce, _, ok := strings.Cut(tok, ".")
	return ok && len(nonce) == 32 && hmac.Equal([]byte(tok), []byte(s.csrfToken(nonce)))
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := s.csrfToken(hex.EncodeToString(b))
	// Secure follows the transport: the mosaic is plain HTTP on loopback only (hostOnly + loopback bind).
	http.SetCookie(w, &http.Cookie{Name: "dxps_csrf", Value: tok, Path: "/", HttpOnly: true, Secure: r.TLS != nil, // #nosec G124
		SameSite: http.SameSiteStrictMode, MaxAge: 12 * 3600})
	writeJSON(w, 200, map[string]string{"csrf": tok})
}

func (s *Server) originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	for _, a := range s.Origins {
		if o == a {
			return true
		}
	}
	return false
}

func (s *Server) mutating(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("dxps_csrf")
		hdr := r.Header.Get("X-CSRF-Token")
		switch {
		case !s.originOK(r):
			writeJSON(w, 403, map[string]string{"error": "origin not allowed"})
		case err != nil || hdr == "" || !hmac.Equal([]byte(c.Value), []byte(hdr)) || !s.csrfValid(hdr):
			writeJSON(w, 403, map[string]string{"error": "csrf token invalid"})
		case !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json"):
			writeJSON(w, 415, map[string]string{"error": "application/json required"})
		default:
			r.Body = http.MaxBytesReader(w, r.Body, 4096)
			h(w, r)
		}
	}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------- snapshot

type Snapshot struct {
	At        time.Time         `json:"at"`
	Tenants   []tenant.Tenant   `json:"tenants"`
	DB        *store.Stats      `json:"db"`
	Topics    []bus.TopicStat   `json:"topics"`
	Lags      []bus.GroupLag    `json:"lags"`
	Orch      json.RawMessage   `json:"orchestrator"`
	Adapter   json.RawMessage   `json:"adapter"`
	EventHub  json.RawMessage   `json:"eventhub"`
	Sims      json.RawMessage   `json:"sims"`
	Exchanges json.RawMessage   `json:"exchanges"`
	Tests     json.RawMessage   `json:"tests"`
	Demo      DemoStatus        `json:"demo"`
	Probes    []ProbeResult     `json:"probes"`
	History   []Point           `json:"history"`
	Catalog   []SpecInfo        `json:"catalog"`
	Up        map[string]bool   `json:"up"`
	Errors    map[string]string `json:"errors,omitempty"`
}

type SpecInfo struct {
	Tenant  string `json:"tenant"`
	Spec    string `json:"spec"`
	Version string `json:"version"`
	TxMode  string `json:"txMode"`
	Prio    string `json:"priority"`
	Tasks   int    `json:"tasks"`
}

func (s *Server) get(ctx context.Context, url, token string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("X-Netsim-Admin", token)
	}
	resp, err := s.local.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil || resp.StatusCode != 200 || !json.Valid(b) {
		return nil, errors.New("bad response " + resp.Status)
	}
	return b, nil
}

func (s *Server) build(ctx context.Context) *Snapshot {
	snap := &Snapshot{At: time.Now(), Up: map[string]bool{}, Errors: map[string]string{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	run := func(name string, f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f()
			mu.Lock()
			snap.Up[name] = err == nil
			if err != nil {
				snap.Errors[name] = err.Error()
			}
			mu.Unlock()
		}()
	}
	run("postgres", func() (err error) {
		if snap.DB, err = s.Store.Stats(ctx); err != nil {
			return err
		}
		snap.Tenants, err = s.Store.LoadTenants(ctx)
		return err
	})
	run("kafka", func() (err error) {
		if s.Admin == nil {
			return errors.New("no admin client")
		}
		if snap.Topics, err = s.Admin.TopicEnds(ctx); err != nil {
			return err
		}
		snap.Lags, err = s.Admin.Lags(ctx)
		return err
	})
	fetch := func(name, url, tok string, dst *json.RawMessage) {
		run(name, func() (err error) { *dst, err = s.get(ctx, url, tok); return })
	}
	fetch("orchestrator", s.Src.Orchestrator, "", &snap.Orch)
	fetch("adapter", s.Src.Adapter, "", &snap.Adapter)
	fetch("eventhub", s.Src.EventHub, "", &snap.EventHub)
	fetch("netsim", s.Src.NetsimAdmin+"/stats", s.Src.NetsimToken, &snap.Sims)
	fetch("exchanges", s.Src.NetsimAdmin+"/exchanges", s.Src.NetsimToken, &snap.Exchanges)
	run("gateway", func() error {
		c, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		defer cancel()
		req, _ := http.NewRequestWithContext(c, http.MethodGet, s.GWURL+"/healthz", nil)
		resp, err := s.Gateway.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return errors.New(resp.Status)
		}
		return nil
	})
	wg.Wait()
	if b, err := os.ReadFile(s.Src.Results); err == nil && json.Valid(b) {
		snap.Tests = b
	}
	delete(snap.Up, "exchanges")
	snap.Demo = s.demo.Status()
	snap.Probes = s.demo.ProbeResults()
	for _, sp := range s.Catalog.All() {
		snap.Catalog = append(snap.Catalog, SpecInfo{sp.Tenant, sp.Code, sp.Version, string(sp.TxMode), sp.Priority().String(), len(sp.Tasks)})
	}
	var lag int64
	for _, l := range snap.Lags {
		lag += max(l.Lag, 0)
	}
	s.mu.Lock()
	if snap.DB != nil {
		s.hist = append(s.hist, Point{T: snap.At.Unix(), Orders: snap.DB.OrdersLastMin, Tasks: snap.DB.TasksLastMin, P99: snap.DB.P99OrderMs, Lag: lag})
		if len(s.hist) > 120 {
			s.hist = s.hist[len(s.hist)-120:]
		}
	}
	snap.History = append([]Point(nil), s.hist...)
	s.mu.Unlock()
	return snap
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if time.Since(s.at) < 900*time.Millisecond && s.cached != nil {
		b := s.cached
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
		return
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	b, _ := json.Marshal(s.build(ctx))
	s.mu.Lock()
	s.cached, s.at = b, time.Now()
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// ---------------------------------------------------------------- fault injection (proxied to netsim admin)

type faultReq struct {
	Sim       string  `json:"sim"`
	LatencyMs int     `json:"latencyMs"`
	ErrorRate float64 `json:"errorRate"`
}

func (s *Server) fault(w http.ResponseWriter, r *http.Request) {
	var f faultReq
	if err := decode(r, &f); err != nil || f.LatencyMs < 0 || f.LatencyMs > 5000 || f.ErrorRate < 0 || f.ErrorRate > 1 {
		writeJSON(w, 400, map[string]string{"error": "invalid fault request"})
		return
	}
	b, _ := json.Marshal(f)
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, s.Src.NetsimAdmin+"/faults", strings.NewReader(string(b)))
	req.Header.Set("X-Netsim-Admin", s.Src.NetsimToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.local.Do(req)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "netsim unreachable"})
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, 4096))
}

// ListenLoopback refuses non-loopback binds: the dashboard can mint tokens for load generation.
func ListenLoopback(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("dashboard must bind to loopback")
	}
	return net.Listen("tcp", addr)
}
