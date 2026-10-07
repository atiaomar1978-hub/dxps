package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dxps/internal/catalog"
	"dxps/internal/registry"
	"dxps/internal/tenant"
)

// Ops-role functions (BYPASSRLS): registry loading, seeding, relay and dashboard statistics.

var ErrNoOps = errors.New("ops pool not configured")

func (s *Store) ops() (*pgxpool.Pool, error) {
	if s.Ops == nil {
		return nil, ErrNoOps
	}
	return s.Ops, nil
}

func (s *Store) LoadTenants(ctx context.Context) ([]tenant.Tenant, error) {
	p, err := s.ops()
	if err != nil {
		return nil, err
	}
	rows, err := p.Query(ctx, `SELECT tenant, name, tenant_type, coalesce(host_tenant,''), sla_tier, sla_weight, quota_tps, status
		FROM tenant ORDER BY tenant`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (tenant.Tenant, error) {
		var t tenant.Tenant
		var ty, st string
		err := r.Scan(&t.ID, &t.Name, &ty, &t.Host, &t.SLATier, &t.SLAWeight, &t.QuotaTPS, &st)
		t.Type, t.Status = tenant.Type(ty), tenant.Status(st)
		return t, err
	})
}

func (s *Store) LoadNEs(ctx context.Context) ([]*registry.NE, error) {
	p, err := s.ops()
	if err != nil {
		return nil, err
	}
	rows, err := p.Query(ctx, `SELECT ne.tenant, ne.id::text, ne.code, ne.domain, coalesce(ne.vendor,''), ne.nf_type, ne.shared,
			ne.max_tps, ne.max_concurrency, ne.reserved_pct,
			coalesce((SELECT jsonb_agg(jsonb_build_object('adapterType', e.adapter_type, 'baseUri', e.base_uri, 'weight', e.weight))
			          FROM ne_endpoint e WHERE e.tenant = ne.tenant AND e.ne_id = ne.id), '[]'),
			coalesce((SELECT jsonb_object_agg(a.tenant, jsonb_build_object('tenant', a.tenant, 'quotaTps', a.quota_tps,
			          'allowedOps', a.allowed_ops, 'subscriberGroup', coalesce(a.subscriber_group,'')))
			          FROM tenant_ne_access a WHERE a.ne_tenant = ne.tenant AND a.ne_id = ne.id), '{}')
		FROM network_element ne ORDER BY ne.code`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*registry.NE, error) {
		ne := &registry.NE{}
		err := r.Scan(&ne.Tenant, &ne.ID, &ne.Code, &ne.Domain, &ne.Vendor, &ne.NFType, &ne.Shared, &ne.MaxTPS, &ne.MaxConc,
			&ne.ReservedPct, &ne.Endpoints, &ne.Access)
		return ne, err
	})
}

// ---------------------------------------------------------------- seed (ops role)

func (s *Store) UpsertTenant(ctx context.Context, t tenant.Tenant) error {
	p, err := s.ops()
	if err != nil {
		return err
	}
	_, err = p.Exec(ctx, `INSERT INTO tenant (tenant, name, tenant_type, host_tenant, sla_tier, sla_weight, quota_tps, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (tenant) DO UPDATE SET name=EXCLUDED.name, tenant_type=EXCLUDED.tenant_type, host_tenant=EXCLUDED.host_tenant,
		  sla_tier=EXCLUDED.sla_tier, sla_weight=EXCLUDED.sla_weight, quota_tps=EXCLUDED.quota_tps, status=EXCLUDED.status`,
		t.ID, t.Name, string(t.Type), nullable(t.Host), t.SLATier, t.SLAWeight, t.QuotaTPS, string(t.Status))
	return err
}

// UpsertNE creates/updates an NE with one endpoint and returns its id.
func (s *Store) UpsertNE(ctx context.Context, ne *registry.NE, authRef string) (string, error) {
	p, err := s.ops()
	if err != nil {
		return "", err
	}
	var id string
	err = p.QueryRow(ctx, `INSERT INTO network_element (tenant, code, domain, vendor, nf_type, shared, max_tps, max_concurrency, reserved_pct)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (tenant, code) DO UPDATE SET domain=EXCLUDED.domain, vendor=EXCLUDED.vendor, nf_type=EXCLUDED.nf_type,
		  shared=EXCLUDED.shared, max_tps=EXCLUDED.max_tps, max_concurrency=EXCLUDED.max_concurrency, reserved_pct=EXCLUDED.reserved_pct
		RETURNING id::text`, ne.Tenant, ne.Code, ne.Domain, ne.Vendor, ne.NFType, ne.Shared, ne.MaxTPS, ne.MaxConc, ne.ReservedPct).Scan(&id)
	if err != nil {
		return "", err
	}
	for _, e := range ne.Endpoints {
		if _, err := p.Exec(ctx, `INSERT INTO ne_endpoint (tenant, ne_id, adapter_type, base_uri, auth_ref, weight)
			VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (tenant, ne_id, adapter_type, base_uri) DO UPDATE SET weight=EXCLUDED.weight`,
			ne.Tenant, id, e.AdapterType, e.BaseURI, authRef, e.Weight); err != nil {
			return "", err
		}
	}
	return id, nil
}

func (s *Store) UpsertAccess(ctx context.Context, neTenant, neID string, a registry.Access) error {
	p, err := s.ops()
	if err != nil {
		return err
	}
	_, err = p.Exec(ctx, `INSERT INTO tenant_ne_access (tenant, ne_tenant, ne_id, quota_tps, allowed_ops, subscriber_group)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (tenant, ne_tenant, ne_id) DO UPDATE SET quota_tps=EXCLUDED.quota_tps,
		  allowed_ops=EXCLUDED.allowed_ops, subscriber_group=EXCLUDED.subscriber_group`,
		a.Tenant, neTenant, neID, a.QuotaTPS, a.AllowedOps, nullable(a.SubscriberGroup))
	return err
}

func (s *Store) UpsertSpec(ctx context.Context, sp *catalog.Spec) error {
	p, err := s.ops()
	if err != nil {
		return err
	}
	body, err := json.Marshal(sp)
	if err != nil {
		return err
	}
	_, err = p.Exec(ctx, `INSERT INTO command_spec (tenant, id, spec_code, version, status, body) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (tenant, id) DO UPDATE SET status=EXCLUDED.status, body=EXCLUDED.body`,
		sp.Tenant, sp.ID, sp.Code, sp.Version, sp.Status, body)
	return err
}

// ---------------------------------------------------------------- dashboard statistics

type Count struct {
	Tenant string `json:"tenant"`
	Key    string `json:"key"`
	Sub    string `json:"sub,omitempty"`
	N      int64  `json:"n"`
}

type NELatency struct {
	NE     string  `json:"ne"`
	Calls  int64   `json:"calls"`
	P50us  float64 `json:"p50us"`
	P99us  float64 `json:"p99us"`
	Errors int64   `json:"errors"`
}

type RecentOrder struct {
	Tenant     string    `json:"tenant"`
	ID         string    `json:"id"`
	ExternalID string    `json:"externalId"`
	State      string    `json:"state"`
	Priority   int       `json:"priority"`
	Spec       string    `json:"spec"`
	At         time.Time `json:"at"`
	DurationMs float64   `json:"durationMs"`
}

type Stats struct {
	OrdersByState []Count       `json:"ordersByState"`
	TasksByDomain []Count       `json:"tasksByDomain"`
	NELatency     []NELatency   `json:"neLatency"`
	Recent        []RecentOrder `json:"recent"`
	OutboxBacklog int64         `json:"outboxBacklog"`
	OrdersLastMin int64         `json:"ordersLastMin"`
	TasksLastMin  int64         `json:"tasksLastMin"`
	P99OrderMs    float64       `json:"p99OrderMs"`
}

func collectCounts(rows pgx.Rows, err error) ([]Count, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Count, error) {
		var c Count
		return c, r.Scan(&c.Tenant, &c.Key, &c.Sub, &c.N)
	})
}

func (s *Store) Stats(ctx context.Context) (*Stats, error) {
	p, err := s.ops()
	if err != nil {
		return nil, err
	}
	st := &Stats{}
	if st.OrdersByState, err = collectCounts(p.Query(ctx, `SELECT tenant, state, '', count(*) FROM service_order GROUP BY 1,2 ORDER BY 1,2`)); err != nil {
		return nil, err
	}
	if st.TasksByDomain, err = collectCounts(p.Query(ctx, `SELECT tenant, domain, state, count(*) FROM ne_task GROUP BY 1,2,3 ORDER BY 1,2,3`)); err != nil {
		return nil, err
	}
	rows, err := p.Query(ctx, `SELECT ne.code, count(*), coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY a.latency_us),0),
			coalesce(percentile_cont(0.99) WITHIN GROUP (ORDER BY a.latency_us),0), count(*) FILTER (WHERE a.outcome <> 'SUCCEEDED')
		FROM task_attempt a JOIN ne_task t ON t.tenant=a.tenant AND t.id=a.task_id
		JOIN network_element ne ON ne.tenant=t.ne_tenant AND ne.id=t.ne_id
		WHERE a.id >= uuid_floor(now() - interval '15 minutes') GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	if st.NELatency, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (NELatency, error) {
		var n NELatency
		return n, r.Scan(&n.NE, &n.Calls, &n.P50us, &n.P99us, &n.Errors)
	}); err != nil {
		return nil, err
	}
	rows, err = p.Query(ctx, `SELECT o.tenant, o.id::text, coalesce(o.external_id,''), o.state, o.priority,
			coalesce((SELECT string_agg(DISTINCT b.spec_code, ',') FROM business_command b WHERE b.tenant=o.tenant AND b.order_id=o.id),''),
			o.requested_at, coalesce(extract(epoch FROM (o.completed_at - o.requested_at)) * 1000, 0)
		FROM service_order o ORDER BY o.requested_at DESC LIMIT 14`)
	if err != nil {
		return nil, err
	}
	if st.Recent, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (RecentOrder, error) {
		var o RecentOrder
		return o, r.Scan(&o.Tenant, &o.ID, &o.ExternalID, &o.State, &o.Priority, &o.Spec, &o.At, &o.DurationMs)
	}); err != nil {
		return nil, err
	}
	err = p.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM outbox WHERE published_at IS NULL),
		(SELECT count(*) FROM service_order WHERE requested_at > now() - interval '1 minute'),
		(SELECT count(*) FROM task_attempt WHERE id >= uuid_floor(now() - interval '1 minute')),
		(SELECT coalesce(percentile_cont(0.99) WITHIN GROUP (ORDER BY extract(epoch FROM (completed_at - requested_at)) * 1000), 0)
		   FROM service_order WHERE completed_at > now() - interval '15 minutes')`).
		Scan(&st.OutboxBacklog, &st.OrdersLastMin, &st.TasksLastMin, &st.P99OrderMs)
	return st, err
}
