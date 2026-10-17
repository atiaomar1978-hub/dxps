package console

import (
	"context"
	"encoding/json"
	"time"

	"dxps/internal/registry"
	"dxps/internal/store"
	"dxps/internal/tenant"
)

// Data is the console's read/write view of the DxPS database (ops role).
type Data interface {
	Stats(ctx context.Context) (*store.Stats, error)
	Tenants(ctx context.Context) ([]TenantRow, error)
	UpdateTenant(ctx context.Context, actor string, t tenant.Tenant) error
	NEs(ctx context.Context) ([]*registry.NE, error)
	UpdateNE(ctx context.Context, actor, neTenant, code string, u NEUpdate) error
	UpdateAccess(ctx context.Context, actor, neTenant, code string, a registry.Access) error
	Orders(ctx context.Context, f OrderFilter) ([]OrderRow, int64, error)
	Order(ctx context.Context, tenantID, id string) (*OrderDetail, error)
	Tasks(ctx context.Context, f TaskFilter) ([]TaskRow, error)
	Audit(ctx context.Context, f AuditFilter) ([]AuditRow, error)
	Hubs(ctx context.Context) ([]HubRow, error)
	Report(ctx context.Context, days int) (*Report, error)
}

type TenantRow struct {
	tenant.Tenant
	Orders   int64      `json:"orders"`
	Failed   int64      `json:"failed"`
	Active   int64      `json:"active"`
	LastSeen *time.Time `json:"lastOrder,omitempty"`
	OwnNEs   int64      `json:"ownNes"`
	Entitled int64      `json:"entitledNes"`
}

type NEUpdate struct {
	MaxTPS      int `json:"maxTps"`
	MaxConc     int `json:"maxConcurrency"`
	ReservedPct int `json:"reservedPct"`
}

type OrderFilter struct {
	Tenant, State, Spec, Query string
	Limit, Offset              int
}

type OrderRow struct {
	Tenant      string     `json:"tenant"`
	ID          string     `json:"id"`
	ExternalID  string     `json:"externalId"`
	State       string     `json:"state"`
	Priority    int        `json:"priority"`
	TxMode      string     `json:"txMode"`
	Channel     string     `json:"channel"`
	Category    string     `json:"category"`
	Specs       string     `json:"specs"`
	Tasks       int64      `json:"tasks"`
	RequestedAt time.Time  `json:"requestedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	DurationMs  float64    `json:"durationMs"`
	ErrorCode   string     `json:"errorCode,omitempty"`
	Error       string     `json:"error,omitempty"`
}

type OrderItem struct {
	ID          string          `json:"id"`
	ItemID      string          `json:"itemId"`
	Spec        string          `json:"spec"`
	SpecVersion string          `json:"specVersion"`
	SpecTenant  string          `json:"specTenant"`
	Action      string          `json:"action"`
	State       string          `json:"state"`
	Priority    int             `json:"priority"`
	TxMode      string          `json:"txMode"`
	EntityKey   string          `json:"entityKey"`
	Params      json.RawMessage `json:"params"`
	DependsOn   []string        `json:"dependsOn"`
	ErrorCode   string          `json:"errorCode,omitempty"`
	Error       string          `json:"error,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
}

type OrderTask struct {
	ID             string          `json:"id"`
	ItemIDs        []string        `json:"itemIds"`
	SpecTask       string          `json:"specTask"`
	NE             string          `json:"ne"`
	NETenant       string          `json:"neTenant"`
	Domain         string          `json:"domain"`
	Operation      string          `json:"operation"`
	State          string          `json:"state"`
	Attempt        int             `json:"attempt"`
	Seq            int             `json:"seq"`
	Compensation   bool            `json:"compensation"`
	CompensatesID  string          `json:"compensatesId,omitempty"`
	AfterPivot     bool            `json:"afterPivot"`
	CoalesceKey    string          `json:"coalesceKey,omitempty"`
	Request        json.RawMessage `json:"request,omitempty"`
	Response       json.RawMessage `json:"response,omitempty"`
	NEStatus       *int            `json:"neStatus,omitempty"`
	NECode         string          `json:"neCode,omitempty"`
	LastError      string          `json:"lastError,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
	DispatchedAt   *time.Time      `json:"dispatchedAt,omitempty"`
	CompletedAt    *time.Time      `json:"completedAt,omitempty"`
	DependsOn      []string        `json:"dependsOn"`
	IdempotencyKey string          `json:"idempotencyKey"`
}

type Attempt struct {
	TaskID    string    `json:"taskId"`
	N         int       `json:"n"`
	StartedAt time.Time `json:"startedAt"`
	LatencyUs int64     `json:"latencyUs"`
	NEStatus  *int      `json:"neStatus,omitempty"`
	NECode    string    `json:"neCode,omitempty"`
	Outcome   string    `json:"outcome"`
	Error     string    `json:"error,omitempty"`
}

type OrderDetail struct {
	OrderRow
	IdempotencyKey string          `json:"idempotencyKey"`
	Raw            json.RawMessage `json:"items"`
	Items          []OrderItem     `json:"businessCommands"`
	Tasks          []OrderTask     `json:"tasks"`
	Attempts       []Attempt       `json:"attempts"`
	Audit          []AuditRow      `json:"audit"`
}

type TaskFilter struct {
	Tenant, State, Domain, NE string
	Limit                     int
}

type TaskRow struct {
	Tenant      string     `json:"tenant"`
	ID          string     `json:"id"`
	OrderID     string     `json:"orderId"`
	ExternalID  string     `json:"externalId"`
	NE          string     `json:"ne"`
	Domain      string     `json:"domain"`
	Operation   string     `json:"operation"`
	State       string     `json:"state"`
	Attempt     int        `json:"attempt"`
	Priority    int        `json:"priority"`
	Comp        bool       `json:"compensation"`
	NEStatus    *int       `json:"neStatus,omitempty"`
	NECode      string     `json:"neCode,omitempty"`
	LastError   string     `json:"lastError,omitempty"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

type AuditFilter struct {
	Tenant, Action, Text string
	Limit                int
}

type AuditRow struct {
	Tenant     string          `json:"tenant"`
	ID         string          `json:"id"`
	At         time.Time       `json:"at"`
	Actor      string          `json:"actor"`
	ActingFor  bool            `json:"actingForTenant"`
	Action     string          `json:"action"`
	ObjectType string          `json:"objectType"`
	ObjectID   string          `json:"objectId,omitempty"`
	Detail     json.RawMessage `json:"detail,omitempty"`
}

type HubRow struct {
	Tenant    string    `json:"tenant"`
	ID        string    `json:"id"`
	Callback  string    `json:"callback"`
	Query     string    `json:"query,omitempty"`
	SecretRef string    `json:"secretRef"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

type DayCount struct {
	Day    string `json:"day"`
	Tenant string `json:"tenant"`
	State  string `json:"state"`
	N      int64  `json:"n"`
}

type SpecStat struct {
	Spec      string  `json:"spec"`
	Items     int64   `json:"items"`
	Completed int64   `json:"completed"`
	Failed    int64   `json:"failed"`
	P50ms     float64 `json:"p50ms"`
	P95ms     float64 `json:"p95ms"`
}

type ErrorStat struct {
	Code   string `json:"code"`
	Tenant string `json:"tenant"`
	N      int64  `json:"n"`
	Sample string `json:"sample"`
}

type Report struct {
	Days    int               `json:"days"`
	ByDay   []DayCount        `json:"byDay"`
	BySpec  []SpecStat        `json:"bySpec"`
	ByNE    []store.NELatency `json:"byNe"`
	Errors  []ErrorStat       `json:"errors"`
	ByState []store.Count     `json:"tasksByDomain"`
}
