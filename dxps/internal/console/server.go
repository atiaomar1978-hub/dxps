// Package console is the DxPS operations and configuration console API (served to the Next.js console
// through its backend-for-frontend). It reads the DxPS database with the ops role, calls the running services
// on loopback, and reuses the DxPS catalog, planner rules and NE registry for the Template Lab.
package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"dxps/internal/catalog"
	"dxps/internal/registry"
	"dxps/internal/tenant"
)

type Server struct {
	Data      Data
	Live      Live
	Users     *Users
	Audit     *AuditLog
	Drafts    *Drafts
	Source    *Source
	Log       *slog.Logger
	LabTenant string
	Info      map[string]any

	mu     sync.Mutex
	cat    *catalog.Catalog
	files  map[string]string
	catErr error
	catAt  time.Time
	reg    *registry.Registry
	regAt  time.Time
}

type ctxKey struct{}

func sessionOf(r *http.Request) *Session { s, _ := r.Context().Value(ctxKey{}).(*Session); return s }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}

func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	s.Log.Error(what, "err", err)
	fail(w, http.StatusBadGateway, what+": "+err.Error())
}

func intParam(r *http.Request, k string, def, lo, hi int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(k))
	if err != nil {
		return def
	}
	return min(max(v, lo), hi)
}

// Handler returns the API mux (all routes under /api/v1).
func (s *Server) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.LabTenant == "" {
		s.LabTenant = "host-mno"
	}
	m := http.NewServeMux()
	route := func(pattern, role string, h http.HandlerFunc) {
		m.HandleFunc(pattern, s.guard(role, h))
	}
	m.HandleFunc("POST /api/v1/auth/login", s.login)
	route("POST /api/v1/auth/logout", "viewer", s.logout)
	route("GET /api/v1/auth/me", "viewer", s.me)
	route("POST /api/v1/auth/password", "viewer", s.password)

	route("GET /api/v1/overview", "viewer", s.overview)
	route("GET /api/v1/orders", "viewer", s.orders)
	route("POST /api/v1/orders", "operator", s.createOrder)
	route("GET /api/v1/orders/{tenant}/{id}", "viewer", s.order)
	route("POST /api/v1/orders/{tenant}/{id}/cancel", "operator", s.cancelOrder)
	route("GET /api/v1/tasks", "viewer", s.tasks)

	route("GET /api/v1/tenants", "viewer", s.tenants)
	route("PUT /api/v1/tenants/{id}", "configurator", s.updateTenant)
	route("GET /api/v1/nes", "viewer", s.nes)
	route("PUT /api/v1/nes/{tenant}/{code}", "configurator", s.updateNE)
	route("PUT /api/v1/nes/{tenant}/{code}/access", "configurator", s.updateAccess)

	route("GET /api/v1/catalog", "viewer", s.catalogList)
	route("GET /api/v1/catalog/spec", "viewer", s.catalogSpec)
	route("GET /api/v1/catalog/files", "viewer", s.catalogFiles)
	route("GET /api/v1/drafts", "viewer", s.draftList)
	route("POST /api/v1/drafts", "configurator", s.draftCreate)
	route("GET /api/v1/drafts/{id}", "viewer", s.draftGet)
	route("PUT /api/v1/drafts/{id}", "configurator", s.draftSave)
	route("DELETE /api/v1/drafts/{id}", "configurator", s.draftDelete)
	route("POST /api/v1/drafts/{id}/validate", "configurator", s.draftValidate)
	route("GET /api/v1/drafts/{id}/export", "configurator", s.draftExport)
	route("POST /api/v1/lab", "configurator", s.lab)

	route("GET /api/v1/kafka", "viewer", s.kafka)
	route("GET /api/v1/dlq", "viewer", s.dlq)
	route("POST /api/v1/dlq/redrive", "operator", s.redrive)
	route("GET /api/v1/sims", "viewer", s.sims)
	route("POST /api/v1/sims/fault", "operator", s.fault)
	route("GET /api/v1/loadgen", "viewer", s.loadgen)
	route("POST /api/v1/loadgen", "operator", s.loadgenStart)
	route("POST /api/v1/probes", "operator", s.probes)
	route("GET /api/v1/tests", "viewer", s.tests)
	route("GET /api/v1/hubs", "viewer", s.hubs)
	route("GET /api/v1/audit", "viewer", s.audit)
	route("GET /api/v1/reports", "viewer", s.reports)
	route("GET /api/v1/settings", "viewer", s.settings)

	route("GET /api/v1/users", "admin", s.userList)
	route("POST /api/v1/users", "admin", s.userAdd)
	route("PUT /api/v1/users/{name}", "admin", s.userUpdate)
	route("DELETE /api/v1/users/{name}", "admin", s.userDelete)

	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	return headers(limits(m))
}

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func limits(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodDelete && r.ContentLength != 0 &&
			!strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			fail(w, http.StatusUnsupportedMediaType, "application/json required")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) guard(role string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sess, found := s.Users.Session(tok)
		if !ok || !found {
			fail(w, http.StatusUnauthorized, "sign in required")
			return
		}
		if !Allows(sess.Roles, role) {
			fail(w, http.StatusForbidden, "requires role "+role)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess)))
	}
}

func (s *Server) record(r *http.Request, action, object string, detail map[string]any) {
	if err := s.Audit.Add(sessionOf(r).User, action, object, detail); err != nil {
		s.Log.Error("audit", "err", err)
	}
}

// actor is the DxPS audit actor for console changes.
func actor(r *http.Request) string { return "console:" + sessionOf(r).User }

// ---------------------------------------------------------------- catalog cache

func (s *Server) catalog() (*catalog.Catalog, map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.catAt) < 3*time.Second && (s.cat != nil || s.catErr != nil) {
		return s.cat, s.files, s.catErr
	}
	s.files, s.catErr = s.Source.Files()
	if s.catErr == nil {
		s.cat, s.catErr = catalog.LoadFS(mapFS(s.files))
	}
	s.catAt = time.Now()
	return s.cat, s.files, s.catErr
}

func (s *Server) registry(ctx context.Context) (*registry.Registry, error) {
	s.mu.Lock()
	if s.reg != nil && time.Since(s.regAt) < 10*time.Second {
		r := s.reg
		s.mu.Unlock()
		return r, nil
	}
	s.mu.Unlock()
	nes, err := s.Data.NEs(ctx)
	if err != nil {
		return nil, err
	}
	r := registry.New(nes...)
	s.mu.Lock()
	s.reg, s.regAt = r, time.Now()
	s.mu.Unlock()
	return r, nil
}

func (s *Server) dropRegistry() {
	s.mu.Lock()
	s.reg = nil
	s.mu.Unlock()
}

// ---------------------------------------------------------------- auth

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &q); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	sess, err := s.Users.Login(strings.TrimSpace(q.Username), q.Password)
	if err != nil {
		_ = s.Audit.Add(q.Username, "auth.login_failed", "", nil)
		fail(w, http.StatusUnauthorized, err.Error())
		return
	}
	_ = s.Audit.Add(sess.User, "auth.login", "", nil)
	writeJSON(w, http.StatusOK, map[string]any{"token": sess.Token, "expires_at": sess.Expires, "username": sess.User, "roles": sess.Roles})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.Users.Logout(tok)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, sessionOf(r)) }

func (s *Server) password(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := decode(r, &q); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Users.ChangePassword(sessionOf(r).User, q.Old, q.New); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.record(r, "auth.password_changed", "", nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- overview

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := map[string]any{"at": time.Now().UTC()}
	errs := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	put := func(k string, v any, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			errs[k] = err.Error()
			return
		}
		out[k] = v
	}
	wg.Go(func() { put("up", s.Live.Health(ctx), nil) })
	wg.Go(func() { st, err := s.Data.Stats(ctx); put("db", st, err) })
	wg.Go(func() { ts, err := s.Data.Tenants(ctx); put("tenants", ts, err) })
	wg.Go(func() { put("services", s.Live.ServiceStats(ctx), nil) })
	wg.Go(func() { k, err := s.Live.Kafka(ctx); put("kafka", k, err) })
	wg.Wait()
	out["errors"] = errs
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- orders and tasks

func (s *Server) orders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := OrderFilter{Tenant: q.Get("tenant"), State: q.Get("state"), Spec: q.Get("spec"), Query: strings.TrimSpace(q.Get("q")),
		Limit: intParam(r, "limit", 50, 1, 500), Offset: intParam(r, "offset", 0, 0, 1_000_000)}
	rows, total, err := s.Data.Orders(r.Context(), f)
	if err != nil {
		s.internal(w, "orders", err)
		return
	}
	if rows == nil {
		rows = []OrderRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "total": total})
}

func (s *Server) order(w http.ResponseWriter, r *http.Request) {
	d, err := s.Data.Order(r.Context(), r.PathValue("tenant"), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		fail(w, http.StatusNotFound, "order not found")
		return
	}
	if err != nil {
		s.internal(w, "order", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type createOrderReq struct {
	Tenant     string         `json:"tenant"`
	ExternalID string         `json:"externalId"`
	Priority   string         `json:"priority"`
	Items      []OrderItemReq `json:"items"`
}

func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
	var q createOrderReq
	if err := decode(r, &q); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if !tenant.Valid(q.Tenant) || len(q.Items) == 0 || len(q.Items) > 20 {
		fail(w, http.StatusBadRequest, "tenant and 1-20 items are required")
		return
	}
	if q.Priority != "" && !slices.Contains([]string{"0", "1", "2", "3"}, q.Priority) {
		fail(w, http.StatusBadRequest, "priority must be 0-3")
		return
	}
	body := SampleOrder(q.ExternalID, q.Items, q.Priority)
	idem := "console-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	st, resp, err := s.Live.Gateway(r.Context(), q.Tenant, actor(r), http.MethodPost, "/serviceOrder", body, idem)
	if err != nil {
		s.internal(w, "gateway", err)
		return
	}
	var id struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(resp, &id)
	s.record(r, "order.create", id.ID, map[string]any{"tenant": q.Tenant, "status": st, "items": len(q.Items)})
	writeJSON(w, http.StatusOK, map[string]any{"status": st, "response": resp, "request": body, "id": id.ID})
}

func (s *Server) cancelOrder(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &q); err != nil && !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "EOF") {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	t, id := r.PathValue("tenant"), r.PathValue("id")
	if !tenant.Valid(t) {
		fail(w, http.StatusBadRequest, "invalid tenant")
		return
	}
	body, _ := json.Marshal(map[string]any{"@type": "CancelServiceOrder", "cancellationReason": q.Reason, "serviceOrder": map[string]string{"id": id}})
	st, resp, err := s.Live.Gateway(r.Context(), t, actor(r), http.MethodPost, "/cancelServiceOrder", body, "")
	if err != nil {
		s.internal(w, "gateway", err)
		return
	}
	s.record(r, "order.cancel", id, map[string]any{"tenant": t, "status": st, "reason": q.Reason})
	writeJSON(w, http.StatusOK, map[string]any{"status": st, "response": resp})
}

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := s.Data.Tasks(r.Context(), TaskFilter{Tenant: q.Get("tenant"), State: q.Get("state"), Domain: q.Get("domain"),
		NE: q.Get("ne"), Limit: intParam(r, "limit", 100, 1, 1000)})
	if err != nil {
		s.internal(w, "tasks", err)
		return
	}
	if rows == nil {
		rows = []TaskRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// ---------------------------------------------------------------- tenants and NEs

func (s *Server) tenants(w http.ResponseWriter, r *http.Request) {
	ts, err := s.Data.Tenants(r.Context())
	if err != nil {
		s.internal(w, "tenants", err)
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

func (s *Server) updateTenant(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Name      string `json:"name"`
		SLATier   string `json:"slaTier"`
		SLAWeight int    `json:"slaWeight"`
		QuotaTPS  int    `json:"quotaTps"`
		Status    string `json:"status"`
	}
	if err := decode(r, &q); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	st := tenant.Status(q.Status)
	switch {
	case strings.TrimSpace(q.Name) == "" || len(q.Name) > 120:
		fail(w, http.StatusBadRequest, "name: 1-120 characters")
		return
	case !slices.Contains([]string{"GOLD", "SILVER", "BRONZE"}, q.SLATier):
		fail(w, http.StatusBadRequest, "slaTier must be GOLD, SILVER or BRONZE")
		return
	case q.SLAWeight < 1 || q.SLAWeight > 64:
		fail(w, http.StatusBadRequest, "slaWeight must be 1-64")
		return
	case q.QuotaTPS < 1 || q.QuotaTPS > 100000:
		fail(w, http.StatusBadRequest, "quotaTps must be 1-100000")
		return
	case !slices.Contains([]tenant.Status{tenant.Active, tenant.Suspended, tenant.Offboarding}, st):
		fail(w, http.StatusBadRequest, "status must be ACTIVE, SUSPENDED or OFFBOARDING")
		return
	}
	id := r.PathValue("id")
	t := tenant.Tenant{ID: id, Name: strings.TrimSpace(q.Name), SLATier: q.SLATier, SLAWeight: q.SLAWeight, QuotaTPS: q.QuotaTPS, Status: st}
	if err := s.Data.UpdateTenant(r.Context(), actor(r), t); errors.Is(err, ErrNotFound) {
		fail(w, http.StatusNotFound, "tenant not found")
		return
	} else if err != nil {
		s.internal(w, "tenant update", err)
		return
	}
	s.record(r, "tenant.update", id, map[string]any{"slaWeight": q.SLAWeight, "quotaTps": q.QuotaTPS, "status": q.Status})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type neView struct {
	*registry.NE
	Calls   int64   `json:"calls15m"`
	Errors  int64   `json:"errors15m"`
	P50us   float64 `json:"p50us"`
	P99us   float64 `json:"p99us"`
	Breaker string  `json:"breaker"`
}

func (s *Server) nes(w http.ResponseWriter, r *http.Request) {
	nes, err := s.Data.NEs(r.Context())
	if err != nil {
		s.internal(w, "nes", err)
		return
	}
	lat := map[string][4]float64{}
	if st, err := s.Data.Stats(r.Context()); err == nil {
		for _, l := range st.NELatency {
			lat[l.NE] = [4]float64{float64(l.Calls), float64(l.Errors), l.P50us, l.P99us}
		}
	}
	brk := map[string]string{}
	if raw, ok := s.Live.ServiceStats(r.Context())["adapter"]; ok {
		var list []struct {
			NE      string `json:"ne"`
			Breaker string `json:"breaker"`
		}
		if json.Unmarshal(raw, &list) == nil {
			for _, x := range list {
				brk[x.NE] = x.Breaker
			}
		}
	}
	out := make([]neView, 0, len(nes))
	for _, n := range nes {
		l := lat[n.Code]
		out = append(out, neView{NE: n, Calls: int64(l[0]), Errors: int64(l[1]), P50us: l[2], P99us: l[3], Breaker: brk[n.Code]})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) updateNE(w http.ResponseWriter, r *http.Request) {
	var q NEUpdate
	if err := decode(r, &q); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if q.MaxTPS < 1 || q.MaxTPS > 100000 || q.MaxConc < 1 || q.MaxConc > 10000 || q.ReservedPct < 0 || q.ReservedPct > 90 {
		fail(w, http.StatusBadRequest, "maxTps 1-100000, maxConcurrency 1-10000, reservedPct 0-90")
		return
	}
	t, code := r.PathValue("tenant"), r.PathValue("code")
	if err := s.Data.UpdateNE(r.Context(), actor(r), t, code, q); errors.Is(err, ErrNotFound) {
		fail(w, http.StatusNotFound, "network element not found")
		return
	} else if err != nil {
		s.internal(w, "ne update", err)
		return
	}
	s.dropRegistry()
	s.record(r, "ne.update", t+"/"+code, map[string]any{"maxTps": q.MaxTPS, "maxConcurrency": q.MaxConc, "reservedPct": q.ReservedPct})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) updateAccess(w http.ResponseWriter, r *http.Request) {
	var q registry.Access
	if err := decode(r, &q); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if !tenant.Valid(q.Tenant) || q.QuotaTPS < 1 || q.QuotaTPS > 100000 || len(q.AllowedOps) == 0 || len(q.AllowedOps) > 32 {
		fail(w, http.StatusBadRequest, "tenant, quotaTps 1-100000 and 1-32 allowed operation patterns are required")
		return
	}
	for _, op := range q.AllowedOps {
		if op == "" || len(op) > 64 || strings.ContainsAny(op, " /") {
			fail(w, http.StatusBadRequest, "invalid operation pattern "+strconv.Quote(op))
			return
		}
	}
	t, code := r.PathValue("tenant"), r.PathValue("code")
	if err := s.Data.UpdateAccess(r.Context(), actor(r), t, code, q); errors.Is(err, ErrNotFound) {
		fail(w, http.StatusNotFound, err.Error())
		return
	} else if err != nil {
		s.internal(w, "entitlement update", err)
		return
	}
	s.dropRegistry()
	s.record(r, "entitlement.upsert", t+"/"+code, map[string]any{"tenant": q.Tenant, "quotaTps": q.QuotaTPS, "allowedOps": q.AllowedOps})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- catalog, drafts, lab

func (s *Server) catalogList(w http.ResponseWriter, r *http.Request) {
	cat, files, err := s.catalog()
	if err != nil {
		s.internal(w, "catalog", err)
		return
	}
	writeJSON(w, http.StatusOK, summarize(cat, files))
}

func (s *Server) catalogSpec(w http.ResponseWriter, r *http.Request) {
	cat, files, err := s.catalog()
	if err != nil {
		s.internal(w, "catalog", err)
		return
	}
	q := r.URL.Query()
	sp := findSpec(cat, q.Get("tenant"), q.Get("spec"), q.Get("version"))
	if sp == nil {
		fail(w, http.StatusNotFound, "spec not found")
		return
	}
	tmpl := map[string]string{}
	for _, t := range sp.Tasks {
		for _, n := range []string{t.Template, t.CompensateTemplate} {
			if n != "" {
				tmpl[n] = files["templates/"+n]
			}
		}
	}
	var versions []string
	for _, o := range cat.All() {
		if o.Tenant == sp.Tenant && o.Code == sp.Code {
			versions = append(versions, o.Version)
		}
	}
	var overrides []string
	for _, o := range cat.All() {
		if o.Code == sp.Code && o.Tenant != sp.Tenant {
			overrides = append(overrides, o.Tenant+" "+o.Version)
		}
	}
	file := specFiles(files)[sp.Tenant+"|"+sp.Code+"|"+sp.Version]
	writeJSON(w, http.StatusOK, map[string]any{"spec": sp, "file": file, "source": files[file], "templates": tmpl,
		"versions": versions, "others": overrides})
}

func (s *Server) catalogFiles(w http.ResponseWriter, r *http.Request) {
	_, files, err := s.catalog()
	if err != nil {
		s.internal(w, "catalog", err)
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (s *Server) draftList(w http.ResponseWriter, r *http.Request) {
	l, err := s.Drafts.List()
	if err != nil {
		s.internal(w, "drafts", err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) draftCreate(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Name     string            `json:"name"`
		Files    map[string]string `json:"files"`
		Override *struct {
			Tenant    string `json:"tenant"`
			Spec      string `json:"spec"`
			Version   string `json:"version"`
			ForTenant string `json:"forTenant"`
		} `json:"override"`
	}
	if err := decode(r, &q); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	files := q.Files
	if q.Override != nil {
		cat, _, err := s.catalog()
		if err != nil {
			s.internal(w, "catalog", err)
			return
		}
		sp := findSpec(cat, q.Override.Tenant, q.Override.Spec, q.Override.Version)
		if sp == nil || !tenant.Valid(q.Override.ForTenant) {
			fail(w, http.StatusBadRequest, "override: spec not found or invalid tenant")
			return
		}
		cp := *sp
		cp.Tenant = q.Override.ForTenant
		y, err := SpecYAML(&cp)
		if err != nil {
			s.internal(w, "yaml", err)
			return
		}
		files = map[string]string{OverrideFile(cp.Tenant, cp.Code): "# Tenant override of " + sp.Tenant + " " + sp.Code + " " + sp.Version + "\n" + y}
	}
	d, err := s.Drafts.Create(q.Name, sessionOf(r).User, files)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.record(r, "draft.create", d.ID, map[string]any{"name": d.Name, "files": d.Summary().Files})
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) draftGet(w http.ResponseWriter, r *http.Request) {
	d, err := s.Drafts.Get(r.PathValue("id"))
	if errors.Is(err, ErrNoDraft) {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		s.internal(w, "draft", err)
		return
	}
	_, files, err := s.catalog()
	if err != nil {
		s.internal(w, "catalog", err)
		return
	}
	base := map[string]string{}
	for p := range d.Files {
		if b, ok := files[p]; ok {
			base[p] = b
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"draft": d, "base": base})
}

func (s *Server) draftSave(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Files   map[string]string `json:"files"`
		Deleted []string          `json:"deleted"`
	}
	if err := decode(r, &q); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	d, err := s.Drafts.Save(r.PathValue("id"), q.Files, q.Deleted)
	if errors.Is(err, ErrNoDraft) {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.record(r, "draft.save", d.ID, map[string]any{"files": d.Summary().Files, "deleted": d.Deleted})
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) draftDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Drafts.Delete(id); errors.Is(err, ErrNoDraft) {
		fail(w, http.StatusNotFound, err.Error())
		return
	} else if err != nil {
		s.internal(w, "draft", err)
		return
	}
	s.record(r, "draft.delete", id, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) draftValidate(w http.ResponseWriter, r *http.Request) {
	d, err := s.Drafts.Get(r.PathValue("id"))
	if errors.Is(err, ErrNoDraft) {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		s.internal(w, "draft", err)
		return
	}
	_, files, err := s.catalog()
	if err != nil {
		s.internal(w, "catalog", err)
		return
	}
	reg, err := s.registry(r.Context())
	if err != nil {
		s.internal(w, "registry", err)
		return
	}
	v := Validate(files, d, reg, s.LabTenant)
	s.record(r, "draft.validate", d.ID, map[string]any{"ok": v.OK})
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) draftExport(w http.ResponseWriter, r *http.Request) {
	d, err := s.Drafts.Get(r.PathValue("id"))
	if errors.Is(err, ErrNoDraft) {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		s.internal(w, "draft", err)
		return
	}
	_, files, err := s.catalog()
	if err != nil {
		s.internal(w, "catalog", err)
		return
	}
	s.record(r, "draft.export", d.ID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"name": d.Name, "files": Changed(files, d), "deleted": d.Deleted,
		"target": "dxps/internal/catalog/data"})
}

func (s *Server) lab(w http.ResponseWriter, r *http.Request) {
	var q LabRequest
	if err := decode(r, &q); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if !tenant.Valid(q.Tenant) || q.Spec == "" {
		fail(w, http.StatusBadRequest, "tenant and spec are required")
		return
	}
	cat, files, err := s.catalog()
	if err != nil {
		s.internal(w, "catalog", err)
		return
	}
	if q.Draft != "" {
		d, err := s.Drafts.Get(q.Draft)
		if err != nil {
			fail(w, http.StatusNotFound, err.Error())
			return
		}
		if cat, err = catalog.LoadFS(mapFS(Overlay(files, d))); err != nil {
			fail(w, http.StatusUnprocessableEntity, "draft does not load: "+err.Error())
			return
		}
	}
	reg, err := s.registry(r.Context())
	if err != nil {
		s.internal(w, "registry", err)
		return
	}
	writeJSON(w, http.StatusOK, RunLab(cat, reg, q))
}

// ---------------------------------------------------------------- messaging, simulators, load

func (s *Server) kafka(w http.ResponseWriter, r *http.Request) {
	k, err := s.Live.Kafka(r.Context())
	if err != nil {
		s.internal(w, "kafka", err)
		return
	}
	writeJSON(w, http.StatusOK, k)
}

func (s *Server) dlq(w http.ResponseWriter, r *http.Request) {
	recs, err := s.Live.DLQ(r.Context(), intParam(r, "limit", 100, 1, 500))
	if err != nil {
		s.internal(w, "dlq", err)
		return
	}
	writeJSON(w, http.StatusOK, recs)
}

func (s *Server) redrive(w http.ResponseWriter, r *http.Request) {
	n, skipped, err := s.Live.Redrive(r.Context())
	s.record(r, "dlq.redrive", "", map[string]any{"replayed": n, "skipped": skipped, "error": errString(err)})
	if err != nil {
		s.internal(w, "redrive", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"replayed": n, "skipped": skipped})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) sims(w http.ResponseWriter, r *http.Request) {
	st, ex, err := s.Live.Sims(r.Context())
	if err != nil {
		s.internal(w, "netsim", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]json.RawMessage{"sims": st, "exchanges": ex})
}

func (s *Server) fault(w http.ResponseWriter, r *http.Request) {
	var f FaultReq
	if err := decode(r, &f); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if f.Sim == "" || f.LatencyMs < 0 || f.LatencyMs > 5000 || f.ErrorRate < 0 || f.ErrorRate > 1 {
		fail(w, http.StatusBadRequest, "sim, latencyMs 0-5000 and errorRate 0-1 are required")
		return
	}
	if err := s.Live.Fault(r.Context(), f); err != nil {
		s.internal(w, "fault", err)
		return
	}
	s.record(r, "sim.fault", f.Sim, map[string]any{"latencyMs": f.LatencyMs, "errorRate": f.ErrorRate})
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) loadgen(w http.ResponseWriter, r *http.Request) {
	st, b, err := s.Live.Mosaic(r.Context(), http.MethodGet, "/api/snapshot", nil)
	if err != nil {
		s.internal(w, "mosaic", err)
		return
	}
	var snap struct {
		Demo    json.RawMessage `json:"demo"`
		Probes  json.RawMessage `json:"probes"`
		History json.RawMessage `json:"history"`
	}
	if st != http.StatusOK || json.Unmarshal(b, &snap) != nil {
		fail(w, http.StatusBadGateway, "mosaic snapshot unavailable")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) loadgenStart(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Count int     `json:"count"`
		Rate  int     `json:"rate"`
		Chaos float64 `json:"chaos"`
	}
	if err := decode(r, &q); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if q.Count < 1 || q.Count > 20000 || q.Rate < 1 || q.Rate > 500 || q.Chaos < 0 || q.Chaos > 1 {
		fail(w, http.StatusBadRequest, "count 1-20000, rate 1-500 per second, chaos 0-1")
		return
	}
	st, b, err := s.Live.Mosaic(r.Context(), http.MethodPost, "/api/demo", q)
	if err != nil {
		s.internal(w, "mosaic", err)
		return
	}
	s.record(r, "loadgen.start", "", map[string]any{"count": q.Count, "rate": q.Rate, "chaos": q.Chaos, "status": st})
	writeRaw(w, st, b)
}

func (s *Server) probes(w http.ResponseWriter, r *http.Request) {
	st, b, err := s.Live.Mosaic(r.Context(), http.MethodPost, "/api/probe", map[string]any{})
	if err != nil {
		s.internal(w, "mosaic", err)
		return
	}
	s.record(r, "probes.start", "", map[string]any{"status": st})
	writeRaw(w, st, b)
}

func writeRaw(w http.ResponseWriter, status int, b json.RawMessage) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func (s *Server) tests(w http.ResponseWriter, r *http.Request) {
	b := s.Live.Tests()
	if b == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	writeRaw(w, http.StatusOK, b)
}

func (s *Server) hubs(w http.ResponseWriter, r *http.Request) {
	h, err := s.Data.Hubs(r.Context())
	if err != nil {
		s.internal(w, "hubs", err)
		return
	}
	if h == nil {
		h = []HubRow{}
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := intParam(r, "limit", 100, 1, 1000)
	if q.Get("source") == "console" {
		l, err := s.Audit.Recent(limit, q.Get("q"))
		if err != nil {
			s.internal(w, "audit", err)
			return
		}
		writeJSON(w, http.StatusOK, l)
		return
	}
	rows, err := s.Data.Audit(r.Context(), AuditFilter{Tenant: q.Get("tenant"), Action: q.Get("action"), Text: strings.TrimSpace(q.Get("q")), Limit: limit})
	if err != nil {
		s.internal(w, "audit", err)
		return
	}
	if rows == nil {
		rows = []AuditRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) reports(w http.ResponseWriter, r *http.Request) {
	rp, err := s.Data.Report(r.Context(), intParam(r, "days", 30, 1, 90))
	if err != nil {
		s.internal(w, "report", err)
		return
	}
	writeJSON(w, http.StatusOK, rp)
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Info)
}

// ---------------------------------------------------------------- users

func (s *Server) userList(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, s.Users.List()) }

func (s *Server) userAdd(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Name     string   `json:"name"`
		Password string   `json:"password"`
		Roles    []string `json:"roles"`
	}
	if err := decode(r, &q); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Users.Add(q.Name, q.Password, q.Roles); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrUserExists) {
			status = http.StatusConflict
		}
		fail(w, status, err.Error())
		return
	}
	s.record(r, "user.add", q.Name, map[string]any{"roles": q.Roles})
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

func (s *Server) userUpdate(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Roles    []string `json:"roles"`
		Disabled bool     `json:"disabled"`
		Password string   `json:"password"`
	}
	if err := decode(r, &q); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	name := r.PathValue("name")
	if err := s.Users.Update(name, q.Roles, q.Disabled, q.Password); errors.Is(err, ErrNoUser) {
		fail(w, http.StatusNotFound, err.Error())
		return
	} else if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.record(r, "user.update", name, map[string]any{"roles": q.Roles, "disabled": q.Disabled, "passwordReset": q.Password != ""})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) userDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == sessionOf(r).User {
		fail(w, http.StatusBadRequest, "you cannot delete your own account")
		return
	}
	if err := s.Users.Delete(name); errors.Is(err, ErrNoUser) {
		fail(w, http.StatusNotFound, err.Error())
		return
	} else if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.record(r, "user.delete", name, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
