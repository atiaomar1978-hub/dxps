package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dxps/internal/registry"
	"dxps/internal/store"
	"dxps/internal/tenant"
)

var ErrNotFound = errors.New("not found")

// PG implements Data with the DxPS ops pool (BYPASSRLS read, limited writes on the inventory tables).
type PG struct {
	St *store.Store
}

func (p *PG) pool() *pgxpool.Pool { return p.St.Ops }

func (p *PG) Stats(ctx context.Context) (*store.Stats, error) { return p.St.Stats(ctx) }

func (p *PG) NEs(ctx context.Context) ([]*registry.NE, error) { return p.St.LoadNEs(ctx) }

func (p *PG) Tenants(ctx context.Context) ([]TenantRow, error) {
	ts, err := p.St.LoadTenants(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := p.pool().Query(ctx, `SELECT t.tenant,
		(SELECT count(*) FROM service_order o WHERE o.tenant = t.tenant),
		(SELECT count(*) FROM service_order o WHERE o.tenant = t.tenant AND o.state IN ('failed','partial')),
		(SELECT count(*) FROM service_order o WHERE o.tenant = t.tenant AND o.state IN ('acknowledged','inProgress','pending','held')),
		(SELECT max(requested_at) FROM service_order o WHERE o.tenant = t.tenant),
		(SELECT count(*) FROM network_element n WHERE n.tenant = t.tenant),
		(SELECT count(*) FROM tenant_ne_access a WHERE a.tenant = t.tenant)
		FROM tenant t`)
	if err != nil {
		return nil, err
	}
	type agg struct{ orders, failed, active, own, ent int64 }
	byID := map[string]agg{}
	last := map[string]*TenantRow{}
	for rows.Next() {
		var id string
		var a agg
		r := &TenantRow{}
		if err := rows.Scan(&id, &a.orders, &a.failed, &a.active, &r.LastSeen, &a.own, &a.ent); err != nil {
			rows.Close()
			return nil, err
		}
		byID[id], last[id] = a, r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]TenantRow, 0, len(ts))
	for _, t := range ts {
		a := byID[t.ID]
		r := TenantRow{Tenant: t, Orders: a.orders, Failed: a.failed, Active: a.active, OwnNEs: a.own, Entitled: a.ent}
		if l := last[t.ID]; l != nil {
			r.LastSeen = l.LastSeen
		}
		out = append(out, r)
	}
	return out, nil
}

func audit(ctx context.Context, tx pgx.Tx, tenantID, actor, action, typ, id string, detail map[string]any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	var oid any
	if id != "" {
		oid = id
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_event (tenant, actor, acting_for_tenant, action, object_type, object_id, detail)
		VALUES ($1,$2,false,$3,$4,$5,$6)`, tenantID, actor, action, typ, oid, b)
	return err
}

func (p *PG) UpdateTenant(ctx context.Context, actor string, t tenant.Tenant) error {
	return pgx.BeginFunc(ctx, p.pool(), func(tx pgx.Tx) error {
		var id string
		var before map[string]any
		err := tx.QueryRow(ctx, `SELECT id::text, jsonb_build_object('name', name, 'slaTier', sla_tier, 'slaWeight', sla_weight,
				'quotaTps', quota_tps, 'status', status) FROM tenant WHERE tenant = $1 FOR UPDATE`, t.ID).Scan(&id, &before)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tenant SET name=$2, sla_tier=$3, sla_weight=$4, quota_tps=$5, status=$6 WHERE tenant=$1`,
			t.ID, t.Name, t.SLATier, t.SLAWeight, t.QuotaTPS, string(t.Status)); err != nil {
			return err
		}
		after := map[string]any{"name": t.Name, "slaTier": t.SLATier, "slaWeight": t.SLAWeight, "quotaTps": t.QuotaTPS, "status": t.Status}
		return audit(ctx, tx, t.ID, actor, "tenant.update", "tenant", id, map[string]any{"before": before, "after": after})
	})
}

func (p *PG) UpdateNE(ctx context.Context, actor, neTenant, code string, u NEUpdate) error {
	return pgx.BeginFunc(ctx, p.pool(), func(tx pgx.Tx) error {
		var id string
		var before map[string]any
		err := tx.QueryRow(ctx, `SELECT id::text, jsonb_build_object('maxTps', max_tps, 'maxConcurrency', max_concurrency,
				'reservedPct', reserved_pct) FROM network_element WHERE tenant=$1 AND code=$2 FOR UPDATE`, neTenant, code).Scan(&id, &before)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE network_element SET max_tps=$3, max_concurrency=$4, reserved_pct=$5 WHERE tenant=$1 AND code=$2`,
			neTenant, code, u.MaxTPS, u.MaxConc, u.ReservedPct); err != nil {
			return err
		}
		return audit(ctx, tx, neTenant, actor, "ne.update", "network_element", id, map[string]any{"code": code, "before": before, "after": u})
	})
}

func (p *PG) UpdateAccess(ctx context.Context, actor, neTenant, code string, a registry.Access) error {
	return pgx.BeginFunc(ctx, p.pool(), func(tx pgx.Tx) error {
		var neID string
		err := tx.QueryRow(ctx, `SELECT id::text FROM network_element WHERE tenant=$1 AND code=$2`, neTenant, code).Scan(&neID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var known bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant WHERE tenant=$1)`, a.Tenant).Scan(&known); err != nil {
			return err
		}
		if !known {
			return fmt.Errorf("%w: tenant %s", ErrNotFound, a.Tenant)
		}
		var group any
		if a.SubscriberGroup != "" {
			group = a.SubscriberGroup
		}
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO tenant_ne_access (tenant, ne_tenant, ne_id, quota_tps, allowed_ops, subscriber_group)
			VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (tenant, ne_tenant, ne_id) DO UPDATE SET quota_tps=EXCLUDED.quota_tps,
			allowed_ops=EXCLUDED.allowed_ops, subscriber_group=EXCLUDED.subscriber_group RETURNING id::text`,
			a.Tenant, neTenant, neID, a.QuotaTPS, a.AllowedOps, group).Scan(&id); err != nil {
			return err
		}
		return audit(ctx, tx, a.Tenant, actor, "entitlement.upsert", "tenant_ne_access", id, map[string]any{"ne": code, "neTenant": neTenant,
			"quotaTps": a.QuotaTPS, "allowedOps": a.AllowedOps, "subscriberGroup": a.SubscriberGroup})
	})
}

const orderCols = `o.tenant, o.id::text, coalesce(o.external_id,''), o.state, o.priority, o.tx_mode, o.channel, coalesce(o.category,''),
	coalesce((SELECT string_agg(DISTINCT b.spec_code, ',') FROM business_command b WHERE b.tenant=o.tenant AND b.order_id=o.id),''),
	(SELECT count(*) FROM ne_task t WHERE t.tenant=o.tenant AND t.order_id=o.id),
	o.requested_at, o.completed_at, coalesce(extract(epoch FROM (o.completed_at - o.requested_at)) * 1000, 0),
	coalesce(o.error_code,''), coalesce(o.error,'')`

func scanOrderRow(r pgx.Row, o *OrderRow) error {
	return r.Scan(&o.Tenant, &o.ID, &o.ExternalID, &o.State, &o.Priority, &o.TxMode, &o.Channel, &o.Category, &o.Specs, &o.Tasks,
		&o.RequestedAt, &o.CompletedAt, &o.DurationMs, &o.ErrorCode, &o.Error)
}

const orderWhere = `WHERE ($1 = '' OR o.tenant = $1) AND ($2 = '' OR o.state = $2)
	AND ($3 = '' OR EXISTS (SELECT 1 FROM business_command b WHERE b.tenant=o.tenant AND b.order_id=o.id AND b.spec_code=$3))
	AND ($4 = '' OR o.id::text = $4 OR o.external_id ILIKE '%' || $4 || '%' OR o.idempotency_key = $4)`

func (p *PG) Orders(ctx context.Context, f OrderFilter) ([]OrderRow, int64, error) {
	var total int64
	if err := p.pool().QueryRow(ctx, `SELECT count(*) FROM service_order o `+orderWhere, f.Tenant, f.State, f.Spec, f.Query).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := p.pool().Query(ctx, `SELECT `+orderCols+` FROM service_order o `+orderWhere+`
		ORDER BY o.requested_at DESC LIMIT $5 OFFSET $6`, f.Tenant, f.State, f.Spec, f.Query, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (OrderRow, error) {
		var o OrderRow
		return o, scanOrderRow(r, &o)
	})
	return out, total, err
}

func (p *PG) Order(ctx context.Context, tenantID, id string) (*OrderDetail, error) {
	d := &OrderDetail{}
	err := scanOrderRow(p.pool().QueryRow(ctx, `SELECT `+orderCols+` FROM service_order o WHERE o.tenant=$1 AND o.id::text=$2`, tenantID, id), &d.OrderRow)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := p.pool().QueryRow(ctx, `SELECT idempotency_key, items FROM service_order WHERE tenant=$1 AND id::text=$2`, tenantID, id).
		Scan(&d.IdempotencyKey, &d.Raw); err != nil {
		return nil, err
	}
	rows, err := p.pool().Query(ctx, `SELECT id::text, order_item_id, spec_code, spec_version, spec_tenant, action, state, priority, tx_mode,
			entity_key, params, depends_on_items, coalesce(error_code,''), coalesce(error,''), created_at, updated_at
		FROM business_command WHERE tenant=$1 AND order_id::text=$2 ORDER BY order_item_id`, tenantID, id)
	if err != nil {
		return nil, err
	}
	if d.Items, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (OrderItem, error) {
		var i OrderItem
		return i, r.Scan(&i.ID, &i.ItemID, &i.Spec, &i.SpecVersion, &i.SpecTenant, &i.Action, &i.State, &i.Priority, &i.TxMode,
			&i.EntityKey, &i.Params, &i.DependsOn, &i.ErrorCode, &i.Error, &i.CreatedAt, &i.UpdatedAt)
	}); err != nil {
		return nil, err
	}
	rows, err = p.pool().Query(ctx, `SELECT t.id::text,
			coalesce((SELECT array_agg(b.order_item_id ORDER BY b.order_item_id) FROM bc_task_link l
			  JOIN business_command b ON b.tenant=l.tenant AND b.id=l.bc_id WHERE l.tenant=t.tenant AND l.task_id=t.id), '{}'),
			coalesce(t.plan->>'specTask',''), coalesce(ne.code,''), t.ne_tenant, t.domain, t.operation, t.state, t.attempt, t.seq,
			t.is_compensation, coalesce(t.compensates_id::text,''), t.after_pivot, coalesce(t.coalesce_key,''), t.request, t.response,
			t.ne_status, coalesce(t.ne_code,''), coalesce(t.last_error,''), t.created_at, t.dispatched_at, t.completed_at,
			coalesce((SELECT array_agg(d.depends_on_id::text) FROM task_dependency d WHERE d.tenant=t.tenant AND d.task_id=t.id), '{}'),
			t.idempotency_key
		FROM ne_task t LEFT JOIN network_element ne ON ne.tenant=t.ne_tenant AND ne.id=t.ne_id
		WHERE t.tenant=$1 AND t.order_id::text=$2 ORDER BY t.is_compensation, t.seq, t.created_at`, tenantID, id)
	if err != nil {
		return nil, err
	}
	if d.Tasks, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (OrderTask, error) {
		var t OrderTask
		return t, r.Scan(&t.ID, &t.ItemIDs, &t.SpecTask, &t.NE, &t.NETenant, &t.Domain, &t.Operation, &t.State, &t.Attempt, &t.Seq,
			&t.Compensation, &t.CompensatesID, &t.AfterPivot, &t.CoalesceKey, &t.Request, &t.Response, &t.NEStatus, &t.NECode,
			&t.LastError, &t.CreatedAt, &t.DispatchedAt, &t.CompletedAt, &t.DependsOn, &t.IdempotencyKey)
	}); err != nil {
		return nil, err
	}
	rows, err = p.pool().Query(ctx, `SELECT a.task_id::text, a.n, a.started_at, coalesce(a.latency_us,0), a.ne_status, coalesce(a.ne_code,''),
			coalesce(a.outcome,''), coalesce(a.error,'')
		FROM task_attempt a WHERE a.tenant=$1 AND a.task_id IN (SELECT t.id FROM ne_task t WHERE t.tenant=$1 AND t.order_id::text=$2)
		ORDER BY a.started_at`, tenantID, id)
	if err != nil {
		return nil, err
	}
	if d.Attempts, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Attempt, error) {
		var a Attempt
		return a, r.Scan(&a.TaskID, &a.N, &a.StartedAt, &a.LatencyUs, &a.NEStatus, &a.NECode, &a.Outcome, &a.Error)
	}); err != nil {
		return nil, err
	}
	d.Audit, err = p.Audit(ctx, AuditFilter{Tenant: tenantID, Text: id, Limit: 50})
	return d, err
}

func (p *PG) Tasks(ctx context.Context, f TaskFilter) ([]TaskRow, error) {
	rows, err := p.pool().Query(ctx, `SELECT t.tenant, t.id::text, t.order_id::text, coalesce(o.external_id,''), coalesce(ne.code,''),
			t.domain, t.operation, t.state, t.attempt, t.priority, t.is_compensation, t.ne_status, coalesce(t.ne_code,''),
			coalesce(t.last_error,''), t.updated_at, t.completed_at
		FROM ne_task t
		LEFT JOIN network_element ne ON ne.tenant=t.ne_tenant AND ne.id=t.ne_id
		LEFT JOIN service_order o ON o.tenant=t.tenant AND o.id=t.order_id
		WHERE ($1 = '' OR t.tenant=$1) AND ($2 = '' OR t.state=$2) AND ($3 = '' OR t.domain=$3) AND ($4 = '' OR ne.code=$4)
		ORDER BY t.updated_at DESC LIMIT $5`, f.Tenant, f.State, f.Domain, f.NE, f.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TaskRow, error) {
		var t TaskRow
		return t, r.Scan(&t.Tenant, &t.ID, &t.OrderID, &t.ExternalID, &t.NE, &t.Domain, &t.Operation, &t.State, &t.Attempt, &t.Priority,
			&t.Comp, &t.NEStatus, &t.NECode, &t.LastError, &t.UpdatedAt, &t.CompletedAt)
	})
}

func (p *PG) Audit(ctx context.Context, f AuditFilter) ([]AuditRow, error) {
	rows, err := p.pool().Query(ctx, `SELECT tenant, id::text, uuid_extract_timestamp(id), actor, acting_for_tenant, action, object_type,
			coalesce(object_id::text,''), detail
		FROM audit_event
		WHERE ($1 = '' OR tenant=$1) AND ($2 = '' OR action=$2)
		  AND ($3 = '' OR object_id::text=$3 OR actor ILIKE '%' || $3 || '%' OR detail::text ILIKE '%' || $3 || '%')
		ORDER BY id DESC LIMIT $4`, f.Tenant, f.Action, f.Text, f.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditRow, error) {
		var a AuditRow
		return a, r.Scan(&a.Tenant, &a.ID, &a.At, &a.Actor, &a.ActingFor, &a.Action, &a.ObjectType, &a.ObjectID, &a.Detail)
	})
}

func (p *PG) Hubs(ctx context.Context) ([]HubRow, error) {
	rows, err := p.pool().Query(ctx, `SELECT tenant, id::text, callback, coalesce(query,''), secret_ref, status, created_at
		FROM hub_subscription ORDER BY tenant, created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (HubRow, error) {
		var h HubRow
		return h, r.Scan(&h.Tenant, &h.ID, &h.Callback, &h.Query, &h.SecretRef, &h.Status, &h.CreatedAt)
	})
}

func (p *PG) Report(ctx context.Context, days int) (*Report, error) {
	rp := &Report{Days: days}
	since := fmt.Sprintf("%d days", days)
	rows, err := p.pool().Query(ctx, `SELECT to_char(date_trunc('day', requested_at), 'YYYY-MM-DD'), tenant, state, count(*)
		FROM service_order WHERE requested_at > now() - $1::interval GROUP BY 1,2,3 ORDER BY 1,2,3`, since)
	if err != nil {
		return nil, err
	}
	if rp.ByDay, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (DayCount, error) {
		var d DayCount
		return d, r.Scan(&d.Day, &d.Tenant, &d.State, &d.N)
	}); err != nil {
		return nil, err
	}
	rows, err = p.pool().Query(ctx, `SELECT b.spec_code, count(*), count(*) FILTER (WHERE b.state='completed'),
			count(*) FILTER (WHERE b.state IN ('failed','partial')),
			coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM (o.completed_at - o.requested_at)) * 1000), 0),
			coalesce(percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM (o.completed_at - o.requested_at)) * 1000), 0)
		FROM business_command b JOIN service_order o ON o.tenant=b.tenant AND o.id=b.order_id
		WHERE o.requested_at > now() - $1::interval GROUP BY 1 ORDER BY 2 DESC`, since)
	if err != nil {
		return nil, err
	}
	if rp.BySpec, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (SpecStat, error) {
		var s SpecStat
		return s, r.Scan(&s.Spec, &s.Items, &s.Completed, &s.Failed, &s.P50ms, &s.P95ms)
	}); err != nil {
		return nil, err
	}
	rows, err = p.pool().Query(ctx, `SELECT ne.code, count(*), coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY a.latency_us),0),
			coalesce(percentile_cont(0.99) WITHIN GROUP (ORDER BY a.latency_us),0), count(*) FILTER (WHERE a.outcome <> 'SUCCEEDED')
		FROM task_attempt a JOIN ne_task t ON t.tenant=a.tenant AND t.id=a.task_id
		JOIN network_element ne ON ne.tenant=t.ne_tenant AND ne.id=t.ne_id
		WHERE a.id >= uuid_floor(now() - $1::interval) GROUP BY 1 ORDER BY 1`, since)
	if err != nil {
		return nil, err
	}
	if rp.ByNE, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (store.NELatency, error) {
		var n store.NELatency
		return n, r.Scan(&n.NE, &n.Calls, &n.P50us, &n.P99us, &n.Errors)
	}); err != nil {
		return nil, err
	}
	rows, err = p.pool().Query(ctx, `SELECT coalesce(error_code,'(none)'), tenant, count(*), coalesce(max(error),'')
		FROM service_order WHERE state IN ('failed','partial','rejected') AND requested_at > now() - $1::interval
		GROUP BY 1,2 ORDER BY 3 DESC LIMIT 50`, since)
	if err != nil {
		return nil, err
	}
	if rp.Errors, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (ErrorStat, error) {
		var e ErrorStat
		return e, r.Scan(&e.Code, &e.Tenant, &e.N, &e.Sample)
	}); err != nil {
		return nil, err
	}
	rows, err = p.pool().Query(ctx, `SELECT tenant, domain, state, count(*) FROM ne_task
		WHERE created_at > now() - $1::interval GROUP BY 1,2,3 ORDER BY 1,2,3`, since)
	if err != nil {
		return nil, err
	}
	rp.ByState, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (store.Count, error) {
		var c store.Count
		return c, r.Scan(&c.Tenant, &c.Key, &c.Sub, &c.N)
	})
	return rp, err
}
