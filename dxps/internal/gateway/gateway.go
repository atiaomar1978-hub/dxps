// Package gateway is the DxPS northbound API: TMF641 Service Ordering and TMF688 event hub over
// TLS 1.3 with OAuth2 bearer tokens carrying the tenant.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/time/rate"

	"dxps/internal/auth"
	"dxps/internal/catalog"
	"dxps/internal/contract"
	"dxps/internal/ids"
	"dxps/internal/saga"
	"dxps/internal/secretbox"
	"dxps/internal/store"
	"dxps/internal/tenant"
)

const (
	BasePath = "/tmf-api/serviceOrdering/v5"
	MaxBody  = 256 << 10
	MaxItems = 50
	MaxChars = 50
)

type Publisher interface {
	Publish(ctx context.Context, rows []*store.OutboxRow) error
}

type Gateway struct {
	Store    *store.Store
	Catalog  *catalog.Catalog
	Tenants  *tenant.Registry
	Signer   *auth.Signer
	Pub      Publisher
	Box      *secretbox.Box
	HubAllow []string // allowed callback host:port
	Log      *slog.Logger

	mu   sync.Mutex
	lims map[string]*rate.Limiter
}

type principal struct {
	claims    *auth.Claims
	tenant    string
	actingFor bool
}

type ctxKey struct{}

func from(r *http.Request) *principal { p, _ := r.Context().Value(ctxKey{}).(*principal); return p }

var (
	reItemID  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	reChar    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,31}$`)
	reExt     = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
	reChannel = regexp.MustCompile(`^[A-Z_]{1,20}$`)
	actions   = map[string]bool{"add": true, "modify": true, "delete": true, "noChange": true}
	states    = map[string]bool{"acknowledged": true, "inProgress": true, "pending": true, "held": true, "completed": true,
		"failed": true, "partial": true, "cancelled": true, "rejected": true}
)

// Handler returns the HTTP handler with the full middleware chain.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+BasePath+"/serviceOrder", g.scope(auth.ScopeOrderWrite, g.createOrder))
	mux.HandleFunc("GET "+BasePath+"/serviceOrder", g.scope(auth.ScopeOrderRead, g.listOrders))
	mux.HandleFunc("GET "+BasePath+"/serviceOrder/{id}", g.scope(auth.ScopeOrderRead, g.getOrder))
	mux.HandleFunc("POST "+BasePath+"/cancelServiceOrder", g.scope(auth.ScopeOrderWrite, g.cancelOrder))
	mux.HandleFunc("POST "+BasePath+"/hub", g.scope(auth.ScopeHubWrite, g.createHub))
	mux.HandleFunc("DELETE "+BasePath+"/hub/{id}", g.scope(auth.ScopeHubWrite, g.deleteHub))
	return g.secure(mux)
}

func (g *Gateway) log() *slog.Logger {
	if g.Log != nil {
		return g.Log
	}
	return slog.Default()
}

// secure: security headers, panic recovery, body limit, health, authentication, tenant resolution, rate limit.
func (g *Gateway) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		defer func() {
			if v := recover(); v != nil {
				g.log().Error("panic", "err", v)
				writeErr(w, http.StatusInternalServerError, "DXPS-9000", "internal error", "")
			}
		}()
		if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
			w.Write([]byte("ok"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
		tok, ok := auth.BearerToken(r.Header.Get("Authorization"))
		if !ok {
			h.Set("WWW-Authenticate", `Bearer realm="dxps"`)
			writeErr(w, http.StatusUnauthorized, "DXPS-1000", "missing bearer token", "")
			return
		}
		cl, err := g.Signer.Verify(tok)
		if err != nil {
			h.Set("WWW-Authenticate", `Bearer realm="dxps", error="invalid_token"`)
			writeErr(w, http.StatusUnauthorized, "DXPS-1000", "invalid token", err.Error())
			return
		}
		p := &principal{claims: cl, tenant: cl.Tenant}
		if xt := r.Header.Get("X-Tenant"); xt != "" && xt != cl.Tenant {
			if !cl.HasScope(auth.ScopeTenantAny) {
				writeErr(w, http.StatusForbidden, "DXPS-1004", "X-Tenant requires scope "+auth.ScopeTenantAny, "")
				return
			}
			if !tenant.Valid(xt) {
				writeErr(w, http.StatusForbidden, "DXPS-1004", "invalid X-Tenant", "")
				return
			}
			p.tenant, p.actingFor = xt, true
		}
		t, err := g.Tenants.Require(p.tenant)
		if err != nil {
			writeErr(w, http.StatusForbidden, "DXPS-1004", "tenant not allowed", err.Error())
			return
		}
		if !g.limiter(t).Allow() {
			h.Set("Retry-After", "1")
			writeErr(w, http.StatusTooManyRequests, "DXPS-1006", "tenant quota exceeded", "")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

func (g *Gateway) limiter(t tenant.Tenant) *rate.Limiter {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lims == nil {
		g.lims = map[string]*rate.Limiter{}
	}
	l := g.lims[t.ID]
	if l == nil || int(l.Limit()) != t.QuotaTPS {
		l = rate.NewLimiter(rate.Limit(t.QuotaTPS), max(1, t.QuotaTPS/5))
		g.lims[t.ID] = l
	}
	return l
}

func (g *Gateway) scope(s string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p := from(r); p == nil || !p.claims.HasScope(s) {
			writeErr(w, http.StatusForbidden, "DXPS-1000", "insufficient scope", "requires "+s)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, reason, msg string) {
	writeJSON(w, status, APIError{Type: "Error", Code: code, Reason: reason, Message: msg, Status: strconv.Itoa(status)})
}

// decodeStrict decodes exactly one JSON object, rejecting unknown fields and trailing data.
func decodeStrict(r *http.Request, v any) ([]byte, error) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return nil, errors.New("Content-Type must be application/json")
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after JSON object")
	}
	return body, nil
}

func bodyErr(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		writeErr(w, http.StatusRequestEntityTooLarge, "DXPS-1002", "request body too large", "")
		return
	}
	writeErr(w, http.StatusBadRequest, "DXPS-1002", "invalid request body", err.Error())
}

func (g *Gateway) audit(ctx context.Context, tx pgx.Tx, p *principal, action, typ, id string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["tokenTenant"] = p.claims.Tenant
	detail["jti"] = p.claims.ID
	return store.InsertAudit(ctx, tx, p.tenant, store.Audit{Actor: p.claims.Subject, ActingFor: p.actingFor,
		Action: action, ObjectType: typ, ObjectID: id, Detail: detail})
}

func (g *Gateway) publish(ctx context.Context, rows []*store.OutboxRow) {
	if g.Pub != nil && len(rows) > 0 {
		if err := g.Pub.Publish(ctx, rows); err != nil {
			g.log().Warn("fast publish failed; relay will publish", "err", err)
		}
	}
}

// ---------------------------------------------------------------- TMF641

type plannedItem struct {
	item   OrderItem
	spec   *catalog.Spec
	params map[string]any
	keys   []string
	prio   contract.Priority
	deps   []string
}

func (g *Gateway) validateOrder(ten string, req *ServiceOrderReq) ([]plannedItem, contract.Priority, int, string, error) {
	fail := func(s int, f string, a ...any) ([]plannedItem, contract.Priority, int, string, error) {
		return nil, 0, s, "DXPS-1002", fmt.Errorf(f, a...)
	}
	if n := len(req.ServiceOrderItem); n == 0 || n > MaxItems {
		return fail(400, "serviceOrderItem must contain 1..%d items", MaxItems)
	}
	if req.ExternalID != "" && !reExt.MatchString(req.ExternalID) {
		return fail(400, "invalid externalId")
	}
	var orderPrio *contract.Priority
	if req.Priority != "" {
		p, err := contract.ParsePriority(req.Priority)
		if err != nil {
			return fail(400, "invalid priority")
		}
		orderPrio = &p
	}
	seen := map[string]int{}
	out := make([]plannedItem, 0, len(req.ServiceOrderItem))
	minPrio := contract.P3
	for i, it := range req.ServiceOrderItem {
		if !reItemID.MatchString(it.ID) {
			return fail(400, "item %d: invalid id", i)
		}
		if _, dup := seen[it.ID]; dup {
			return fail(400, "duplicate item id %s", it.ID)
		}
		seen[it.ID] = i
		if !actions[it.Action] {
			return fail(400, "item %s: invalid action", it.ID)
		}
		spec, err := g.Catalog.Resolve(ten, it.Service.ServiceSpecification.ID, it.Service.ServiceSpecification.Version)
		if err != nil {
			return nil, 0, 400, "DXPS-1001", fmt.Errorf("item %s: %w", it.ID, err)
		}
		if len(it.Service.ServiceCharacteristic) > MaxChars {
			return fail(400, "item %s: too many characteristics", it.ID)
		}
		params := map[string]any{}
		for _, c := range it.Service.ServiceCharacteristic {
			if !reChar.MatchString(c.Name) {
				return fail(400, "item %s: invalid characteristic name", it.ID)
			}
			if _, dup := params[c.Name]; dup {
				return fail(400, "item %s: duplicate characteristic %s", it.ID, c.Name)
			}
			var v any
			if err := json.Unmarshal(c.Value, &v); err != nil {
				return fail(400, "item %s: characteristic %s: invalid value", it.ID, c.Name)
			}
			params[c.Name] = v
		}
		if err := spec.Validate(params); err != nil {
			return nil, 0, 400, "DXPS-1002", fmt.Errorf("item %s: %w", it.ID, err)
		}
		keys, err := spec.EntityKeysFor(ten, params)
		if err != nil {
			return nil, 0, 400, "DXPS-1002", fmt.Errorf("item %s: %w", it.ID, err)
		}
		prio := spec.Priority()
		if orderPrio != nil {
			prio = *orderPrio
		}
		minPrio = min(minPrio, prio)
		var deps []string
		for _, rel := range it.ServiceOrderItemRelationship {
			if rel.RelationshipType != "dependsOn" {
				continue
			}
			deps = append(deps, rel.OrderItem.ItemID)
		}
		out = append(out, plannedItem{item: it, spec: spec, params: params, keys: keys, prio: prio, deps: deps})
	}
	// dependsOn must reference other items and be acyclic
	for _, pi := range out {
		for _, d := range pi.deps {
			if _, ok := seen[d]; !ok || d == pi.item.ID {
				return fail(400, "item %s: invalid dependsOn %s", pi.item.ID, d)
			}
		}
	}
	if cyclic(out) {
		return fail(400, "serviceOrderItemRelationship contains a cycle")
	}
	return out, minPrio, 0, "", nil
}

func cyclic(items []plannedItem) bool {
	deps := map[string][]string{}
	for _, it := range items {
		deps[it.item.ID] = it.deps
	}
	state := map[string]int{}
	var visit func(string) bool
	visit = func(n string) bool {
		switch state[n] {
		case 1:
			return true
		case 2:
			return false
		}
		state[n] = 1
		for _, d := range deps[n] {
			if visit(d) {
				return true
			}
		}
		state[n] = 2
		return false
	}
	for n := range deps {
		if visit(n) {
			return true
		}
	}
	return false
}

func (g *Gateway) createOrder(w http.ResponseWriter, r *http.Request) {
	p := from(r)
	idem := r.Header.Get("Idempotency-Key")
	if !ids.ValidIdempotencyKey(idem) {
		writeErr(w, http.StatusBadRequest, "DXPS-1002", "Idempotency-Key header required (8-64 chars [A-Za-z0-9_.:-])", "")
		return
	}
	var req ServiceOrderReq
	body, err := decodeStrict(r, &req)
	if err != nil {
		bodyErr(w, err)
		return
	}
	items, prio, status, code, err := g.validateOrder(p.tenant, &req)
	if err != nil {
		writeErr(w, status, code, "order validation failed", err.Error())
		return
	}
	channel := "API"
	if len(req.Channel) > 0 && reChannel.MatchString(req.Channel[0].Name) {
		channel = req.Channel[0].Name
	}
	sum := sha256.Sum256(body)
	orderID := ids.Derive("order", p.tenant, idem).String()
	echo := make([]OrderItem, len(items))
	for i, it := range items {
		echo[i] = it.item
		echo[i].State = "acknowledged"
	}
	itemsJSON, _ := json.Marshal(echo)
	var rows []*store.OutboxRow
	var created bool
	err = g.Store.InTenant(r.Context(), p.tenant, func(tx pgx.Tx) error {
		created, err = store.CreateOrder(r.Context(), tx, store.NewOrder{Tenant: p.tenant, ID: orderID, ExternalID: req.ExternalID,
			Channel: channel, Category: req.Category, Priority: int(prio), TxMode: string(items[0].spec.TxMode),
			IdempotencyKey: idem, RequestHash: sum[:], Items: itemsJSON})
		if err != nil || !created {
			return err
		}
		for _, it := range items {
			bc := contract.BusinessCommand{Tenant: p.tenant, MessageID: ids.Derive("msg", p.tenant, idem, it.item.ID).String(),
				OrderID: orderID, OrderItemID: it.item.ID, BCID: ids.Derive("bc", p.tenant, idem, it.item.ID).String(),
				EntityKey: it.keys[0], ExtraEntityKeys: it.keys[1:], CommandSpec: it.spec.Code, SpecVersion: it.spec.Version,
				Action: it.item.Action, Priority: it.prio, TxMode: it.spec.TxMode, Params: it.params, DependsOnItems: it.deps,
				RequestedStart: req.RequestedStartDate, Deadline: req.RequestedCompletionDate, Channel: channel}
			payload, _ := json.Marshal(bc)
			rows = append(rows, &store.OutboxRow{Tenant: p.tenant, Topic: contract.BCTopic(it.prio),
				Key: contract.Key(p.tenant, it.keys[0]), Payload: payload, Headers: map[string]string{
					contract.HdrTenant: p.tenant, contract.HdrMessageID: bc.MessageID, contract.HdrPriority: it.prio.String(),
					contract.HdrSchema: "dxps.v1.BusinessCommand"}})
		}
		if err := store.InsertOutbox(r.Context(), tx, rows); err != nil {
			return err
		}
		if p.actingFor {
			return g.audit(r.Context(), tx, p, "serviceOrder.create", "service_order", orderID,
				map[string]any{"externalId": req.ExternalID, "items": len(items)})
		}
		return nil
	})
	if errors.Is(err, store.ErrConflict) {
		writeErr(w, http.StatusConflict, "DXPS-1008", "idempotency key or externalId reused with a different request", "")
		return
	}
	if err != nil {
		g.log().Error("create order", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "DXPS-9001", "order store unavailable", "")
		return
	}
	g.publish(r.Context(), rows)
	g.respondOrder(w, r, p.tenant, orderID, map[bool]int{true: http.StatusCreated, false: http.StatusOK}[created], !created)
}

func (g *Gateway) respondOrder(w http.ResponseWriter, r *http.Request, ten, id string, status int, replay bool) {
	var o *store.OrderView
	err := g.Store.InTenant(r.Context(), ten, func(tx pgx.Tx) error {
		var err error
		o, err = store.GetOrder(r.Context(), tx, id)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "DXPS-1404", "service order not found", "")
		return
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "DXPS-9001", "order store unavailable", "")
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Location", BasePath+"/serviceOrder/"+id)
	writeJSON(w, status, toTMF(o))
}

func toTMF(o *store.OrderView) ServiceOrder {
	var items []OrderItem
	_ = json.Unmarshal(o.Items, &items)
	st := map[string]store.ItemState{}
	for _, s := range o.ItemStates {
		st[s.ItemID] = s
	}
	so := ServiceOrder{Type: "ServiceOrder", ID: o.ID, Href: BasePath + "/serviceOrder/" + o.ID, ExternalID: o.ExternalID,
		Priority: strconv.Itoa(o.Priority), Category: o.Category, State: o.State, OrderDate: o.RequestedAt,
		CompletionDate: o.CompletedAt, ServiceOrderItem: items}
	for i := range so.ServiceOrderItem {
		if s, ok := st[so.ServiceOrderItem[i].ID]; ok {
			so.ServiceOrderItem[i].State = s.State
			if s.ErrorCode != "" {
				so.ErrorMessage = append(so.ErrorMessage, ErrorMessage{Type: "ServiceOrderErrorMessage", Code: s.ErrorCode,
					Reason: "item " + s.ItemID, Message: s.Error})
			}
		}
	}
	if o.ErrorCode != "" && len(so.ErrorMessage) == 0 {
		so.ErrorMessage = append(so.ErrorMessage, ErrorMessage{Type: "ServiceOrderErrorMessage", Code: o.ErrorCode, Reason: "order", Message: o.Error})
	}
	if so.ServiceOrderItem == nil {
		so.ServiceOrderItem = []OrderItem{}
	}
	return so
}

func (g *Gateway) getOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ids.ValidUUID(id) {
		writeErr(w, http.StatusNotFound, "DXPS-1404", "service order not found", "")
		return
	}
	g.respondOrder(w, r, from(r).tenant, id, http.StatusOK, false)
}

func (g *Gateway) listOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	if state != "" && !states[state] {
		writeErr(w, http.StatusBadRequest, "DXPS-1002", "invalid state filter", "")
		return
	}
	limit, offset := 20, 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeErr(w, http.StatusBadRequest, "DXPS-1002", "limit must be 1..100", "")
			return
		}
		limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100000 {
			writeErr(w, http.StatusBadRequest, "DXPS-1002", "invalid offset", "")
			return
		}
		offset = n
	}
	var list []*store.OrderView
	err := g.Store.InTenant(r.Context(), from(r).tenant, func(tx pgx.Tx) error {
		var err error
		list, err = store.ListOrders(r.Context(), tx, state, limit, offset)
		return err
	})
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "DXPS-9001", "order store unavailable", "")
		return
	}
	out := make([]ServiceOrder, len(list))
	for i, o := range list {
		out[i] = toTMF(o)
	}
	w.Header().Set("X-Result-Count", strconv.Itoa(len(out)))
	writeJSON(w, http.StatusOK, out)
}

func (g *Gateway) cancelOrder(w http.ResponseWriter, r *http.Request) {
	p := from(r)
	var req CancelReq
	if _, err := decodeStrict(r, &req); err != nil {
		bodyErr(w, err)
		return
	}
	if !ids.ValidUUID(req.ServiceOrder.ID) {
		writeErr(w, http.StatusBadRequest, "DXPS-1002", "serviceOrder.id must be a uuid", "")
		return
	}
	var o *store.OrderView
	var rows []*store.OutboxRow
	err := g.Store.InTenant(r.Context(), p.tenant, func(tx pgx.Tx) error {
		var err error
		if o, err = store.CancelOrder(r.Context(), tx, req.ServiceOrder.ID); err != nil {
			return err
		}
		ev := contract.OrderEvent{EventID: ids.NewString(), EventTime: time.Now().UTC(), EventType: "ServiceOrderStateChangeEvent"}
		ev.Event.ServiceOrder = contract.OrderRef{ID: o.ID, ExternalID: o.ExternalID, State: saga.Cancelled, CompletionDate: o.CompletedAt}
		b, _ := json.Marshal(ev)
		rows = []*store.OutboxRow{{Tenant: p.tenant, Topic: contract.TopicEvent, Key: contract.Key(p.tenant, o.ID), Payload: b,
			Headers: map[string]string{contract.HdrTenant: p.tenant, contract.HdrPriority: "p1", contract.HdrMessageID: ev.EventID}}}
		if err := store.InsertOutbox(r.Context(), tx, rows); err != nil {
			return err
		}
		return g.audit(r.Context(), tx, p, "serviceOrder.cancel", "service_order", o.ID, map[string]any{"reason": req.CancellationReason})
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "DXPS-1404", "service order not found", "")
		return
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "DXPS-1009", "order cannot be cancelled in its current state", "")
		return
	case err != nil:
		writeErr(w, http.StatusServiceUnavailable, "DXPS-9001", "order store unavailable", "")
		return
	}
	g.publish(r.Context(), rows)
	writeJSON(w, http.StatusCreated, map[string]any{"@type": "CancelServiceOrder", "id": ids.NewString(), "state": "done",
		"cancellationReason": req.CancellationReason, "serviceOrder": map[string]string{"id": o.ID, "href": BasePath + "/serviceOrder/" + o.ID}})
}

// ---------------------------------------------------------------- TMF688 hub

// callbackAllowed enforces https and an explicit host:port allowlist (SSRF protection).
func (g *Gateway) callbackAllowed(cb string) bool {
	u, err := url.Parse(cb)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || len(cb) > 512 || u.Fragment != "" {
		return false
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}
	for _, a := range g.HubAllow {
		if strings.EqualFold(a, host) {
			return true
		}
	}
	return false
}

func (g *Gateway) createHub(w http.ResponseWriter, r *http.Request) {
	p := from(r)
	var req HubReq
	if _, err := decodeStrict(r, &req); err != nil {
		bodyErr(w, err)
		return
	}
	if !g.callbackAllowed(req.Callback) {
		writeErr(w, http.StatusBadRequest, "DXPS-1002", "callback must be an allow-listed https URL", "")
		return
	}
	if len(req.Query) > 256 {
		writeErr(w, http.StatusBadRequest, "DXPS-1002", "query too long", "")
		return
	}
	secret, generated := req.Secret, false
	if secret == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			writeErr(w, http.StatusInternalServerError, "DXPS-9000", "internal error", "")
			return
		}
		secret, generated = base64.RawURLEncoding.EncodeToString(b), true
	} else if len(secret) < 16 || len(secret) > 128 {
		writeErr(w, http.StatusBadRequest, "DXPS-1002", "secret must be 16..128 characters", "")
		return
	}
	ref, err := g.Box.Seal(p.tenant, []byte(secret))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DXPS-9000", "internal error", "")
		return
	}
	h := &store.Hub{Tenant: p.tenant, Callback: req.Callback, Query: req.Query, SecretRef: ref}
	err = g.Store.InTenant(r.Context(), p.tenant, func(tx pgx.Tx) error {
		if err := store.InsertHub(r.Context(), tx, h); err != nil {
			return err
		}
		return g.audit(r.Context(), tx, p, "hub.create", "hub_subscription", h.ID, map[string]any{"callback": req.Callback})
	})
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "DXPS-9001", "store unavailable", "")
		return
	}
	resp := HubResp{ID: h.ID, Callback: h.Callback, Query: h.Query}
	if generated {
		resp.Secret = secret
	}
	w.Header().Set("Location", BasePath+"/hub/"+h.ID)
	writeJSON(w, http.StatusCreated, resp)
}

func (g *Gateway) deleteHub(w http.ResponseWriter, r *http.Request) {
	p := from(r)
	id := r.PathValue("id")
	if !ids.ValidUUID(id) {
		writeErr(w, http.StatusNotFound, "DXPS-1404", "hub not found", "")
		return
	}
	err := g.Store.InTenant(r.Context(), p.tenant, func(tx pgx.Tx) error {
		if err := store.DeleteHub(r.Context(), tx, id); err != nil {
			return err
		}
		return g.audit(r.Context(), tx, p, "hub.delete", "hub_subscription", id, nil)
	})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "DXPS-1404", "hub not found", "")
		return
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "DXPS-9001", "store unavailable", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SortedAllow returns the configured allowlist (for logging).
func (g *Gateway) SortedAllow() []string {
	out := append([]string(nil), g.HubAllow...)
	sort.Strings(out)
	return out
}
