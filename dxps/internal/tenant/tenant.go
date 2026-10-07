// Package tenant holds the tenant model, validation and the in-memory tenant registry.
package tenant

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"sync"
)

// Global is the reserved tenant for shared catalog defaults.
const Global = "GLOBAL"

var pattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,40}$`)

// Valid reports whether s satisfies the dxps_tenant domain (varchar(40), safe charset).
func Valid(s string) bool { return pattern.MatchString(s) }

type Type string

const (
	Host       Type = "HOST"
	FullMVNO   Type = "FULL_MVNO"
	LightMVNO  Type = "LIGHT_MVNO"
	Enterprise Type = "ENTERPRISE"
)

type Status string

const (
	Active      Status = "ACTIVE"
	Suspended   Status = "SUSPENDED"
	Offboarding Status = "OFFBOARDING"
)

type Tenant struct {
	ID        string `json:"tenant"`
	Name      string `json:"name"`
	Type      Type   `json:"tenantType"`
	Host      string `json:"hostTenant,omitempty"`
	SLATier   string `json:"slaTier"`
	SLAWeight int    `json:"slaWeight"`
	QuotaTPS  int    `json:"quotaTps"`
	Status    Status `json:"status"`
}

func (t Tenant) Active() bool { return t.Status == Active }

var (
	ErrUnknown   = errors.New("DXPS-1004: unknown tenant")
	ErrInactive  = errors.New("DXPS-1004: tenant not active")
	ErrInvalidID = errors.New("DXPS-1004: invalid tenant identifier")
)

type ctxKey struct{}

func WithTenant(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

func FromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKey{}).(string)
	return v, ok && v != ""
}

// Registry is a concurrency-safe snapshot of tenants (loaded from PostgreSQL, refreshed periodically).
type Registry struct {
	mu sync.RWMutex
	m  map[string]Tenant
}

func NewRegistry(ts ...Tenant) *Registry {
	r := &Registry{m: map[string]Tenant{}}
	r.Replace(ts)
	return r
}

func (r *Registry) Replace(ts []Tenant) {
	m := make(map[string]Tenant, len(ts))
	for _, t := range ts {
		m[t.ID] = t
	}
	r.mu.Lock()
	r.m = m
	r.mu.Unlock()
}

func (r *Registry) Get(id string) (Tenant, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.m[id]
	return t, ok
}

// Require returns the tenant if it exists and is active.
func (r *Registry) Require(id string) (Tenant, error) {
	if !Valid(id) {
		return Tenant{}, ErrInvalidID
	}
	t, ok := r.Get(id)
	if !ok {
		return Tenant{}, ErrUnknown
	}
	if !t.Active() {
		return Tenant{}, ErrInactive
	}
	return t, nil
}

func (r *Registry) All() []Tenant {
	r.mu.RLock()
	out := make([]Tenant, 0, len(r.m))
	for _, t := range r.m {
		out = append(out, t)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Quantum is the DRR quantum of a tenant (SLA weight, minimum 1).
func (r *Registry) Quantum(id string) int {
	if t, ok := r.Get(id); ok && t.SLAWeight > 0 {
		return t.SLAWeight
	}
	return 1
}
