package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"

	"dxps/internal/config"
	"dxps/internal/store"
	"dxps/internal/tenant"
)

// traceQueries return one jsonb array each. They run as dxps_app inside InTenant, so RLS limits every row to the
// requested tenant (ne_code is null for NEs owned by another tenant); tableoid shows the partition holding the row.
var traceQueries = []struct{ name, sql string }{
	{"service_order", `SELECT coalesce(jsonb_agg(x), '[]') FROM (
		SELECT tableoid::regclass::text AS partition, tenant, id, external_id, channel, state, priority, tx_mode,
		       idempotency_key, items, requested_at, completed_at, error_code, error, version
		FROM service_order WHERE id = $1) x`},
	{"business_command", `SELECT coalesce(jsonb_agg(x ORDER BY x.order_item_id), '[]') FROM (
		SELECT tableoid::regclass::text AS partition, tenant, id, order_item_id, message_id, entity_key, entity_seq,
		       spec_tenant, spec_code, spec_version, action, state, priority, tx_mode, params, depends_on_items, error_code
		FROM business_command WHERE order_id = $1) x`},
	{"ne_task", `SELECT coalesce(jsonb_agg(x ORDER BY x.created_at, x.seq), '[]') FROM (
		SELECT t.tableoid::regclass::text AS partition, t.tenant, t.id, t.ne_tenant, t.ne_id, ne.code AS ne_code, t.domain,
		       t.operation, t.state, t.attempt, t.in_degree, t.priority, t.seq, t.is_compensation, t.compensates_id,
		       t.idempotency_key, t.coalesce_key, t.request, t.response, t.ne_status, t.ne_code AS result_code,
		       t.created_at, t.dispatched_at, t.completed_at
		FROM ne_task t LEFT JOIN network_element ne ON ne.tenant = t.ne_tenant AND ne.id = t.ne_id
		WHERE t.order_id = $1) x`},
	{"task_dependency", `SELECT coalesce(jsonb_agg(x), '[]') FROM (
		SELECT d.tenant, d.id, d.task_id, t.operation AS task_op, d.depends_on_id, p.operation AS depends_on_op, d.kind
		FROM task_dependency d
		JOIN ne_task t ON t.tenant = d.tenant AND t.id = d.task_id
		JOIN ne_task p ON p.tenant = d.tenant AND p.id = d.depends_on_id
		WHERE d.order_id = $1) x`},
	{"bc_task_link", `SELECT coalesce(jsonb_agg(x), '[]') FROM (
		SELECT tenant, id, bc_id, task_id FROM bc_task_link WHERE order_id = $1) x`},
	{"task_attempt", `SELECT coalesce(jsonb_agg(x ORDER BY x.started_at), '[]') FROM (
		SELECT a.tableoid::regclass::text AS partition, a.tenant, a.id, a.task_id, t.operation, a.n, a.started_at,
		       a.latency_us, a.ne_status, a.ne_code, a.outcome, a.error
		FROM task_attempt a JOIN ne_task t ON t.tenant = a.tenant AND t.id = a.task_id
		WHERE t.order_id = $1) x`},
	{"entity_sequencer", `SELECT coalesce(jsonb_agg(x), '[]') FROM (
		SELECT s.tableoid::regclass::text AS partition, s.tenant, s.id, s.entity_key, s.next_seq, s.active_bc_id, s.updated_at
		FROM entity_sequencer s WHERE s.entity_key IN (SELECT entity_key FROM business_command WHERE order_id = $1)) x`},
	{"outbox", `SELECT coalesce(jsonb_agg(x ORDER BY x.id), '[]') FROM (
		SELECT tenant, id, shard, topic, convert_from(key, 'UTF8') AS key, headers,
		       convert_from(payload, 'UTF8')::jsonb AS payload, created_at, published_at
		FROM outbox WHERE convert_from(key, 'UTF8') LIKE '%' || $1::text || '%'
		   OR convert_from(payload, 'UTF8') LIKE '%' || $1::text || '%') x`},
}

// cmdTrace prints every database row DxPS holds for one order (read-only, as the order's tenant).
// usage: dxpsctl trace -tenant mvno-beta -order <uuid>
func cmdTrace(args []string) error {
	fs := flag.NewFlagSet("trace", flag.ExitOnError)
	ten := fs.String("tenant", "", "tenant that owns the order")
	order := fs.String("order", "", "service order id (uuid)")
	fs.Parse(args)
	if !tenant.Valid(*ten) || *order == "" {
		return fmt.Errorf("usage: dxpsctl trace -tenant t -order uuid")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	st, err := store.Open(c, cfg.PGApp, "")
	if err != nil {
		return err
	}
	defer st.Close()
	out := map[string]json.RawMessage{}
	err = st.InTenant(c, *ten, func(tx pgx.Tx) error {
		if _, err := tx.Exec(c, "SET TRANSACTION READ ONLY"); err != nil {
			return err
		}
		for _, q := range traceQueries {
			var v []byte
			if err := tx.QueryRow(c, q.sql, *order).Scan(&v); err != nil {
				return fmt.Errorf("%s: %w", q.name, err)
			}
			out[q.name] = v
		}
		return nil
	})
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
