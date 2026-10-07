// Package registry is the in-memory NE registry: NE selection limited to NEs the tenant owns or is
// entitled to (tenant_ne_access), and the per-NE / per-tenant budgets applied before every NE call.
package registry

import (
	"context"
	"errors"
	"path"
	"sort"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"dxps/internal/breaker"
	"dxps/internal/contract"
)

var (
	ErrNoNE        = errors.New("DXPS-1003: no NE / endpoint resolvable")
	ErrNotEntitled = errors.New("DXPS-1005: tenant not entitled to the NE or operation")
	ErrCircuitOpen = errors.New("circuit open for NE")
)

type Endpoint struct {
	AdapterType string `json:"adapterType"`
	BaseURI     string `json:"baseUri"`
	Weight      int    `json:"weight"`
}

type Access struct {
	Tenant          string   `json:"tenant"`
	QuotaTPS        int      `json:"quotaTps"`
	AllowedOps      []string `json:"allowedOps"`
	SubscriberGroup string   `json:"subscriberGroup,omitempty"`
}

// Allows reports whether op matches one of the glob patterns (e.g. "udr.*").
func (a Access) Allows(op string) bool {
	for _, p := range a.AllowedOps {
		if ok, _ := path.Match(p, op); ok {
			return true
		}
	}
	return false
}

type NE struct {
	Tenant      string            `json:"tenant"`
	ID          string            `json:"id"`
	Code        string            `json:"code"`
	Domain      string            `json:"domain"`
	NFType      string            `json:"nfType"`
	Vendor      string            `json:"vendor"`
	Shared      bool              `json:"shared"`
	MaxTPS      int               `json:"maxTps"`
	MaxConc     int               `json:"maxConcurrency"`
	ReservedPct int               `json:"reservedPct"`
	Endpoints   []Endpoint        `json:"endpoints"`
	Access      map[string]Access `json:"access"` // tenant -> entitlement on this (shared) NE

	limiter   *rate.Limiter // NE-wide budget for P2/P3
	reserved  *rate.Limiter // reserved share for P0/P1
	tenantLim map[string]*rate.Limiter
	conc      chan struct{}
	Breaker   *breaker.Breaker `json:"-"`
}

func (ne *NE) init() {
	tps := ne.MaxTPS
	if tps <= 0 {
		tps = 100
	}
	res := tps * ne.ReservedPct / 100
	ne.limiter = rate.NewLimiter(rate.Limit(tps-res), max(1, (tps-res)/10))
	ne.reserved = rate.NewLimiter(rate.Limit(max(res, 1)), max(1, res/10))
	ne.tenantLim = map[string]*rate.Limiter{}
	for t, a := range ne.Access {
		q := max(a.QuotaTPS, 1)
		ne.tenantLim[t] = rate.NewLimiter(rate.Limit(q), max(1, q/10))
	}
	c := ne.MaxConc
	if c <= 0 {
		c = 64
	}
	ne.conc = make(chan struct{}, c)
	if ne.Breaker == nil {
		ne.Breaker = breaker.New(5, 5*time.Second)
	}
}

// Entitled reports whether tenant may run op on this NE.
func (ne *NE) Entitled(tenantID, op string) (Access, bool) {
	if ne.Tenant == tenantID {
		return Access{Tenant: tenantID, AllowedOps: []string{"*"}}, true
	}
	a, ok := ne.Access[tenantID]
	return a, ok && a.Allows(op)
}

// Acquire applies the tenant's quota, then the NE-wide budget (P0/P1 may use the reserved share),
// then the concurrency cap. The returned release func must be called after the NE call.
func (ne *NE) Acquire(ctx context.Context, tenantID, op string, p contract.Priority) (func(), error) {
	if _, ok := ne.Entitled(tenantID, op); !ok {
		return nil, ErrNotEntitled
	}
	if !ne.Breaker.Allow() {
		return nil, ErrCircuitOpen
	}
	if l, ok := ne.tenantLim[tenantID]; ok {
		if err := l.Wait(ctx); err != nil {
			return nil, err
		}
	}
	if !(p <= contract.P1 && ne.reserved.Allow()) {
		if err := ne.limiter.Wait(ctx); err != nil {
			return nil, err
		}
	}
	select {
	case ne.conc <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return func() { <-ne.conc }, nil
}

// Endpoint returns the endpoint to use (highest weight; health-based rotation is done by the adapter).
func (ne *NE) Endpoint() (Endpoint, error) {
	if len(ne.Endpoints) == 0 {
		return Endpoint{}, ErrNoNE
	}
	return ne.Endpoints[0], nil
}

type Registry struct {
	mu   sync.RWMutex
	byID map[string]*NE // ne_tenant|id
	all  []*NE
}

func New(nes ...*NE) *Registry {
	r := &Registry{}
	r.Replace(nes)
	return r
}

func (r *Registry) Replace(nes []*NE) {
	by := make(map[string]*NE, len(nes))
	for _, ne := range nes {
		ne.init()
		sort.SliceStable(ne.Endpoints, func(i, j int) bool { return ne.Endpoints[i].Weight > ne.Endpoints[j].Weight })
		by[ne.Tenant+"|"+ne.ID] = ne
	}
	sort.Slice(nes, func(i, j int) bool { return nes[i].Code < nes[j].Code })
	r.mu.Lock()
	r.byID, r.all = by, nes
	r.mu.Unlock()
}

func (r *Registry) Get(neTenant, id string) (*NE, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ne, ok := r.byID[neTenant+"|"+id]
	return ne, ok
}

func (r *Registry) All() []*NE {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]*NE(nil), r.all...)
}

// Selection is the chosen NE plus the entitlement under which the tenant uses it.
type Selection struct {
	NE     *NE
	Access Access
}

// Select picks an NE of nfType for tenant: its own NE first (FULL_MVNO with own core / OCS),
// otherwise a shared NE it is entitled to for op.
func (r *Registry) Select(tenantID, nfType, op string) (Selection, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var shared []Selection
	found := false
	for _, ne := range r.all {
		if ne.NFType != nfType {
			continue
		}
		found = true
		if ne.Tenant == tenantID {
			a, _ := ne.Entitled(tenantID, op)
			return Selection{NE: ne, Access: a}, nil
		}
		if a, ok := ne.Entitled(tenantID, op); ok {
			shared = append(shared, Selection{NE: ne, Access: a})
		}
	}
	if len(shared) > 0 {
		return shared[0], nil
	}
	if found {
		return Selection{}, ErrNotEntitled
	}
	return Selection{}, ErrNoNE
}
