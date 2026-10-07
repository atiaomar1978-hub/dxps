package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"dxps/internal/contract"
	"dxps/internal/planner"
)

// Task states.
const (
	TaskBlocked     = "BLOCKED"
	TaskReady       = "READY"
	TaskSucceeded   = "SUCCEEDED"
	TaskFailed      = "FAILED"
	TaskCompensated = "COMPENSATED"
	TaskSkipped     = "SKIPPED"
)

// BC states (TMF641 item states plus compensating).
const (
	BCPending      = "pending"
	BCInProgress   = "inProgress"
	BCCompensating = "compensating"
	BCCompleted    = "completed"
	BCFailed       = "failed"
	BCCancelled    = "cancelled"
	BCRejected     = "rejected"
)

func BCTerminal(s string) bool {
	return s == BCCompleted || s == BCFailed || s == BCCancelled || s == BCRejected
}

// InboxInsert records a consumed message id; dup=true when it was already processed.
func InboxInsert(ctx context.Context, tx pgx.Tx, tenantID, messageID string) (dup bool, err error) {
	ct, err := tx.Exec(ctx, `INSERT INTO orchestrator_inbox (tenant, id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, tenantID, messageID)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() == 0, nil
}

// EnsureOrder creates a minimal order for BCs produced natively to Kafka (no gateway order row).
func EnsureOrder(ctx context.Context, tx pgx.Tx, bc *contract.BusinessCommand) error {
	_, err := tx.Exec(ctx, `INSERT INTO service_order (tenant, id, channel, state, priority, tx_mode, idempotency_key,
			request_hash, items, requested_at)
		VALUES ($1,$2::uuid,$3,'acknowledged',$4,$5,$6,'\x00','[]',now()) ON CONFLICT DO NOTHING`,
		bc.Tenant, bc.OrderID, bc.Channel, int(bc.Priority), string(bc.TxMode), "bc:"+bc.OrderID)
	return err
}

func LockOrder(ctx context.Context, tx pgx.Tx, tenantID, orderID string) (string, error) {
	var st string
	err := tx.QueryRow(ctx, `SELECT state FROM service_order WHERE tenant=$1 AND id=$2 FOR UPDATE`, tenantID, orderID).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return st, err
}

type BC struct {
	Tenant, ID, OrderID, ItemID, MessageID string
	EntityKeys                             []string
	EntitySeq                              int64
	SpecID, SpecTenant, SpecCode, SpecVer  string
	Action, State                          string
	Priority                               contract.Priority
	TxMode                                 contract.TxMode
	Params                                 map[string]any
	DependsOnItems                         []string
	Preempt                                bool
}

// ToCommand rebuilds the contract message from a stored BC (used when a parked BC is admitted).
func (b *BC) ToCommand() *contract.BusinessCommand {
	return &contract.BusinessCommand{
		Tenant: b.Tenant, MessageID: b.MessageID, OrderID: b.OrderID, OrderItemID: b.ItemID, BCID: b.ID,
		EntityKey: b.EntityKeys[0], ExtraEntityKeys: b.EntityKeys[1:], EntitySeq: b.EntitySeq,
		CommandSpec: b.SpecCode, SpecVersion: b.SpecVer, Action: b.Action, Priority: b.Priority,
		TxMode: b.TxMode, Params: b.Params, DependsOnItems: b.DependsOnItems,
	}
}

// InsertBC stores a business command (state pending) and its entity rows; inserted=false if it exists.
func InsertBC(ctx context.Context, tx pgx.Tx, b *BC) (bool, error) {
	ct, err := tx.Exec(ctx, `INSERT INTO business_command (tenant, id, order_id, order_item_id, message_id, entity_key, entity_keys,
			entity_seq, spec_id, spec_tenant, spec_code, spec_version, action, state, priority, tx_mode, params, depends_on_items, preempt)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'pending',$14,$15,$16,$17,$18) ON CONFLICT DO NOTHING`,
		b.Tenant, b.ID, b.OrderID, b.ItemID, b.MessageID, b.EntityKeys[0], b.EntityKeys, b.EntitySeq,
		b.SpecID, b.SpecTenant, b.SpecCode, b.SpecVer, b.Action, int(b.Priority), string(b.TxMode), b.Params,
		nonNil(b.DependsOnItems), b.Preempt)
	if err != nil || ct.RowsAffected() == 0 {
		return false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO bc_entity (tenant, bc_id, entity_key) SELECT $1, $2, k FROM unnest($3::text[]) k
		ON CONFLICT DO NOTHING`, b.Tenant, b.ID, b.EntityKeys)
	b.State = BCPending
	return err == nil, err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

const bcColsTpl = `{a}tenant, {a}id::text, {a}order_id::text, {a}order_item_id, {a}message_id::text, {a}entity_keys, {a}entity_seq,
	{a}spec_id::text, {a}spec_tenant, {a}spec_code, {a}spec_version, {a}action, {a}state, {a}priority, {a}tx_mode, {a}params,
	{a}depends_on_items, {a}preempt`

var bcCols = cols(bcColsTpl, "")

func scanBC(r pgx.Row) (*BC, error) {
	b := &BC{}
	var p int
	var m string
	err := r.Scan(&b.Tenant, &b.ID, &b.OrderID, &b.ItemID, &b.MessageID, &b.EntityKeys, &b.EntitySeq, &b.SpecID,
		&b.SpecTenant, &b.SpecCode, &b.SpecVer, &b.Action, &b.State, &p, &m, &b.Params, &b.DependsOnItems, &b.Preempt)
	b.Priority, b.TxMode = contract.Priority(p), contract.TxMode(m)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

func GetBC(ctx context.Context, tx pgx.Tx, tenantID, id string, lock bool) (*BC, error) {
	q := `SELECT ` + bcCols + ` FROM business_command WHERE tenant=$1 AND id=$2`
	if lock {
		q += ` FOR UPDATE`
	}
	return scanBC(tx.QueryRow(ctx, q, tenantID, id))
}

func SetBCState(ctx context.Context, tx pgx.Tx, tenantID, id, state, code, msg string) error {
	_, err := tx.Exec(ctx, `UPDATE business_command SET state=$3, error_code=$4, error=$5, updated_at=now()
		WHERE tenant=$1 AND id=$2`, tenantID, id, state, nullable(code), nullable(msg))
	return err
}

func OrderBCs(ctx context.Context, tx pgx.Tx, tenantID, orderID string) ([]*BC, error) {
	rows, err := tx.Query(ctx, `SELECT `+bcCols+` FROM business_command WHERE tenant=$1 AND order_id=$2 ORDER BY order_item_id`, tenantID, orderID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*BC, error) { return scanBC(r) })
}

// ---------------------------------------------------------------- entity sequencer (LLD 6.7)

type Admission int

const (
	Admitted Admission = iota
	Parked
	Stale
)

// Admit tries to make bcID the active command of all its entity keys. Rows are locked in canonical
// (sorted) order so concurrent admissions cannot deadlock.
func Admit(ctx context.Context, tx pgx.Tx, tenantID string, keys []string, bcID string, seq int64) (Admission, error) {
	keys = sortedUnique(keys)
	if _, err := tx.Exec(ctx, `INSERT INTO entity_sequencer (tenant, entity_key) SELECT $1, k FROM unnest($2::text[]) k
		ON CONFLICT (tenant, entity_key) DO NOTHING`, tenantID, keys); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT entity_key, next_seq, coalesce(active_bc_id::text,'') FROM entity_sequencer
		WHERE tenant=$1 AND entity_key = ANY($2::text[]) ORDER BY entity_key FOR UPDATE`, tenantID, keys)
	if err != nil {
		return 0, err
	}
	res := Admitted
	for rows.Next() {
		var k, active string
		var next int64
		if err := rows.Scan(&k, &next, &active); err != nil {
			rows.Close()
			return 0, err
		}
		switch {
		case active == bcID:
		case seq > 0 && seq < next:
			res = Stale
		case active != "" || (seq > 0 && seq > next):
			if res != Stale {
				res = Parked
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || res != Admitted {
		return res, err
	}
	_, err = tx.Exec(ctx, `UPDATE entity_sequencer SET active_bc_id=$3, updated_at=now()
		WHERE tenant=$1 AND entity_key = ANY($2::text[])`, tenantID, keys, bcID)
	return Admitted, err
}

// Release frees the entity keys held by bcID and advances their sequence.
func Release(ctx context.Context, tx pgx.Tx, tenantID string, keys []string, bcID string) error {
	_, err := tx.Exec(ctx, `UPDATE entity_sequencer SET active_bc_id=NULL, next_seq=next_seq+1, updated_at=now()
		WHERE tenant=$1 AND entity_key = ANY($2::text[]) AND active_bc_id=$3`, tenantID, sortedUnique(keys), bcID)
	return err
}

// ParkedFor returns pending BCs waiting on any of keys (pre-emptive first, then sequence, then arrival).
func ParkedFor(ctx context.Context, tx pgx.Tx, tenantID string, keys []string) ([]*BC, error) {
	rows, err := tx.Query(ctx, `SELECT `+cols(bcColsTpl, "b.")+` FROM business_command b
		WHERE b.tenant=$1 AND b.state='pending' AND b.id IN (SELECT e.bc_id FROM bc_entity e WHERE e.tenant=$1 AND e.entity_key = ANY($2::text[]))
		ORDER BY b.preempt DESC, b.entity_seq, b.created_at, b.id LIMIT 50`, tenantID, keys)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*BC, error) { return scanBC(r) })
}

// WaitingOnItems returns pending BCs of the order that depend on another order item.
func WaitingOnItems(ctx context.Context, tx pgx.Tx, tenantID, orderID string) ([]*BC, error) {
	rows, err := tx.Query(ctx, `SELECT `+bcCols+` FROM business_command WHERE tenant=$1 AND order_id=$2 AND state='pending'
		AND cardinality(depends_on_items) > 0 ORDER BY order_item_id`, tenantID, orderID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*BC, error) { return scanBC(r) })
}

// ItemDependency reports the state of the order items a BC depends on: ready (all completed),
// failed (any terminal but not completed) or neither (still waiting).
func ItemDependency(ctx context.Context, tx pgx.Tx, b *BC) (ready, failed bool, err error) {
	if len(b.DependsOnItems) == 0 {
		return true, false, nil
	}
	rows, err := tx.Query(ctx, `SELECT order_item_id, state FROM business_command WHERE tenant=$1 AND order_id=$2
		AND order_item_id = ANY($3::text[])`, b.Tenant, b.OrderID, b.DependsOnItems)
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	done := 0
	for rows.Next() {
		var id, st string
		if err := rows.Scan(&id, &st); err != nil {
			return false, false, err
		}
		if st == BCCompleted {
			done++
		} else if BCTerminal(st) {
			failed = true
		}
	}
	return done == len(b.DependsOnItems), failed, rows.Err()
}

func sortedUnique(xs []string) []string {
	out := append([]string(nil), xs...)
	sort.Strings(out)
	j := 0
	for i, x := range out {
		if i == 0 || x != out[j-1] {
			out[j] = x
			j++
		}
	}
	return out[:j]
}

// ---------------------------------------------------------------- tasks

type Task struct {
	Tenant, ID, OrderID, NETenant, NEID, Domain, Operation, State string
	Attempt, InDegree, Seq                                        int
	Priority                                                      contract.Priority
	IsCompensation, AfterPivot                                    bool
	CompensatesID, IdempotencyKey, CoalesceKey                    string
	Plan                                                          planner.TaskPlan
	Request, Response                                             json.RawMessage
	NEStatus                                                      int
	NECode, LastError                                             string
}

func (t *Task) Terminal() bool {
	return t.State == TaskSucceeded || t.State == TaskFailed || t.State == TaskCompensated || t.State == TaskSkipped
}

const taskColsTpl = `{a}tenant, {a}id::text, {a}order_id::text, {a}ne_tenant, {a}ne_id::text, {a}domain, {a}operation, {a}state,
	{a}attempt, {a}in_degree, {a}seq, {a}priority, {a}is_compensation, {a}after_pivot, coalesce({a}compensates_id::text,''),
	{a}idempotency_key, coalesce({a}coalesce_key,''), {a}plan, {a}request, {a}response, coalesce({a}ne_status,0),
	coalesce({a}ne_code,''), coalesce({a}last_error,'')`

var taskCols = cols(taskColsTpl, "")

func cols(tpl, alias string) string { return strings.ReplaceAll(tpl, "{a}", alias) }

func scanTask(r pgx.Row) (*Task, error) {
	t := &Task{}
	var p int
	err := r.Scan(&t.Tenant, &t.ID, &t.OrderID, &t.NETenant, &t.NEID, &t.Domain, &t.Operation, &t.State, &t.Attempt,
		&t.InDegree, &t.Seq, &p, &t.IsCompensation, &t.AfterPivot, &t.CompensatesID, &t.IdempotencyKey, &t.CoalesceKey,
		&t.Plan, &t.Request, &t.Response, &t.NEStatus, &t.NECode, &t.LastError)
	t.Priority = contract.Priority(p)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

func collectTasks(rows pgx.Rows, err error) ([]*Task, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Task, error) { return scanTask(r) })
}

func GetTask(ctx context.Context, tx pgx.Tx, tenantID, id string, lock bool) (*Task, error) {
	q := `SELECT ` + taskCols + ` FROM ne_task WHERE tenant=$1 AND id=$2`
	if lock {
		q += ` FOR UPDATE`
	}
	return scanTask(tx.QueryRow(ctx, q, tenantID, id))
}

// CoalesceCandidates maps coalesce keys of not-yet-dispatched forward tasks of the order to task ids.
func CoalesceCandidates(ctx context.Context, tx pgx.Tx, tenantID, orderID string) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT coalesce_key, id::text FROM ne_task WHERE tenant=$1 AND order_id=$2
		AND coalesce_key IS NOT NULL AND state='BLOCKED' AND NOT is_compensation`, tenantID, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, id string
		if err := rows.Scan(&k, &id); err != nil {
			return nil, err
		}
		out[k] = id
	}
	return out, rows.Err()
}

// InsertPlan persists the plan of bcID: new tasks, coalesced merges, M:N links and DAG edges.
// It returns the tasks that are ready now (in_degree 0) - the caller publishes them via the outbox.
func InsertPlan(ctx context.Context, tx pgx.Tx, bcID string, p *planner.Plan) ([]*Task, error) {
	bc := p.BC
	b := &pgx.Batch{}
	var ready []*Task
	for _, t := range p.Tasks {
		if t.MergedInto != "" {
			b.Queue(`UPDATE ne_task SET plan = jsonb_set(plan, '{params}', (plan->'params') || $3::jsonb), updated_at=now()
				WHERE tenant=$1 AND id=$2 AND state='BLOCKED'`, bc.Tenant, t.MergedInto, bc.Params)
			b.Queue(`INSERT INTO bc_task_link (tenant, order_id, bc_id, task_id, contribution) VALUES ($1,$2,$3,$4,$5)
				ON CONFLICT DO NOTHING`, bc.Tenant, bc.OrderID, bcID, t.MergedInto, bc.Params)
			continue
		}
		state := TaskBlocked
		if t.Ready() {
			state = TaskReady
		}
		var req any
		if t.Request != nil {
			req = t.Request
		}
		var ck any
		if t.CoalesceKey != "" {
			ck = t.CoalesceKey
		}
		b.Queue(`INSERT INTO ne_task (tenant, id, order_id, ne_tenant, ne_id, domain, operation, state, in_degree, priority, seq,
				after_pivot, idempotency_key, coalesce_key, plan, request, dispatched_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16, CASE WHEN $8='READY' THEN now() END)`,
			bc.Tenant, t.ID, bc.OrderID, t.NE.Tenant, t.NE.ID, t.Plan.Domain, t.Op, state, len(t.DependsOn), int(t.Priority),
			t.Seq, t.Pivot, t.IdempotencyKey, ck, t.Plan, req)
		b.Queue(`INSERT INTO bc_task_link (tenant, order_id, bc_id, task_id) VALUES ($1,$2,$3,$4)`, bc.Tenant, bc.OrderID, bcID, t.ID)
		for _, d := range t.DependsOn {
			b.Queue(`INSERT INTO task_dependency (tenant, order_id, task_id, depends_on_id) VALUES ($1,$2,$3,$4)`,
				bc.Tenant, bc.OrderID, t.ID, d)
		}
		if state == TaskReady {
			ready = append(ready, &Task{Tenant: bc.Tenant, ID: t.ID, OrderID: bc.OrderID, NETenant: t.NE.Tenant, NEID: t.NE.ID,
				Domain: t.Plan.Domain, Operation: t.Op, State: state, Priority: t.Priority, Seq: t.Seq, AfterPivot: t.Pivot,
				IdempotencyKey: t.IdempotencyKey, Plan: t.Plan, Request: t.Request})
		}
	}
	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return nil, err
	}
	return ready, nil
}

// RecordAttempt stores the outcome of one NE call (append-only).
func RecordAttempt(ctx context.Context, tx pgx.Tx, t *Task, r *contract.TaskResult) error {
	_, err := tx.Exec(ctx, `INSERT INTO task_attempt (tenant, task_id, n, started_at, latency_us, ne_status, ne_code, outcome, error)
		VALUES ($1,$2,$3, now() - make_interval(secs => $9::float8 / 1e6), $4, $5, $6, $7, $8)`,
		t.Tenant, t.ID, r.Attempt, r.LatencyUS, r.NEStatus, nullable(r.NECode), string(r.Outcome), nullable(r.Message), float64(r.LatencyUS))
	return err
}

// UpdateTask persists state/attempt/response fields of t.
func UpdateTask(ctx context.Context, tx pgx.Tx, t *Task) error {
	var resp any
	if len(t.Response) > 0 {
		resp = t.Response
	}
	var req any
	if len(t.Request) > 0 {
		req = t.Request
	}
	_, err := tx.Exec(ctx, `UPDATE ne_task SET state=$3, attempt=$4, response=$5, ne_status=$6, ne_code=$7, last_error=$8,
			request=$9, idempotency_key=$10, updated_at=now(),
			dispatched_at = CASE WHEN $3='READY' THEN now() ELSE dispatched_at END,
			completed_at = CASE WHEN $3 IN ('SUCCEEDED','FAILED','COMPENSATED','SKIPPED') THEN now() ELSE completed_at END
		WHERE tenant=$1 AND id=$2`, t.Tenant, t.ID, t.State, t.Attempt, resp, t.NEStatus, nullable(t.NECode),
		nullable(t.LastError), req, t.IdempotencyKey)
	return err
}

// UnblockDependants decrements in-degree of tasks depending on taskID and returns those now ready to run.
func UnblockDependants(ctx context.Context, tx pgx.Tx, tenantID, taskID string) ([]*Task, error) {
	return collectTasks(tx.Query(ctx, `UPDATE ne_task t SET in_degree = t.in_degree - 1, updated_at=now()
		FROM task_dependency d WHERE d.tenant=$1 AND d.depends_on_id=$2 AND t.tenant=d.tenant AND t.id=d.task_id
		  AND t.state='BLOCKED'
		RETURNING `+cols(taskColsTpl, "t."), tenantID, taskID))
}

// DependencyResults returns NE responses of the tasks taskID depends on (and of the task it compensates),
// keyed by spec task id, for rendering downstream requests.
func DependencyResults(ctx context.Context, tx pgx.Tx, tenantID, taskID, compensatesID string) (map[string]map[string]any, error) {
	rows, err := tx.Query(ctx, `SELECT t.plan->>'specTask', t.response FROM ne_task t
		WHERE t.tenant=$1 AND t.response IS NOT NULL AND (t.id IN (SELECT depends_on_id FROM task_dependency WHERE tenant=$1 AND task_id=$2)
		  OR t.id = $3::uuid)`, tenantID, taskID, nullUUID(compensatesID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]any{}
	for rows.Next() {
		var k string
		var v map[string]any
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// BCsOfTask returns ids of the business commands linked to a task.
func BCsOfTask(ctx context.Context, tx pgx.Tx, tenantID, taskID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT bc_id::text FROM bc_task_link WHERE tenant=$1 AND task_id=$2 ORDER BY bc_id`, tenantID, taskID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// TasksOfBC returns all tasks linked to a business command with their dependency ids.
func TasksOfBC(ctx context.Context, tx pgx.Tx, tenantID, bcID string) ([]*Task, map[string][]string, error) {
	tasks, err := collectTasks(tx.Query(ctx, `SELECT `+cols(taskColsTpl, "t.")+` FROM ne_task t
		JOIN bc_task_link l ON l.tenant=t.tenant AND l.task_id=t.id WHERE l.tenant=$1 AND l.bc_id=$2 ORDER BY t.seq, t.id`, tenantID, bcID))
	if err != nil {
		return nil, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT d.task_id::text, d.depends_on_id::text FROM task_dependency d
		JOIN bc_task_link l ON l.tenant=d.tenant AND l.task_id=d.task_id WHERE l.tenant=$1 AND l.bc_id=$2`, tenantID, bcID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	deps := map[string][]string{}
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, nil, err
		}
		deps[a] = append(deps[a], b)
	}
	return tasks, deps, rows.Err()
}

// SkipBlocked marks the not-yet-dispatched forward tasks of a BC as skipped.
func SkipBlocked(ctx context.Context, tx pgx.Tx, tenantID, bcID string) error {
	_, err := tx.Exec(ctx, `UPDATE ne_task t SET state='SKIPPED', updated_at=now(), completed_at=now()
		FROM bc_task_link l WHERE l.tenant=$1 AND l.bc_id=$2 AND t.tenant=l.tenant AND t.id=l.task_id
		  AND t.state='BLOCKED' AND NOT t.is_compensation`, tenantID, bcID)
	return err
}

// SkipDependants marks every not-yet-dispatched task that transitively depends on taskID as skipped.
func SkipDependants(ctx context.Context, tx pgx.Tx, tenantID, taskID string) error {
	_, err := tx.Exec(ctx, `WITH RECURSIVE dep(id) AS (
			SELECT task_id FROM task_dependency WHERE tenant=$1 AND depends_on_id=$2
			UNION SELECT d.task_id FROM task_dependency d JOIN dep ON d.depends_on_id = dep.id WHERE d.tenant=$1)
		UPDATE ne_task SET state='SKIPPED', updated_at=now(), completed_at=now()
		WHERE tenant=$1 AND id IN (SELECT id FROM dep) AND state='BLOCKED'`, tenantID, taskID)
	return err
}

// InsertCompensations creates a chain of compensation tasks (reverse order: comp[0] runs first) linked to bcID
// and returns the first (ready) one.
func InsertCompensations(ctx context.Context, tx pgx.Tx, bcID string, comps []*Task) (*Task, error) {
	var prev string
	for i, c := range comps {
		state, in := TaskBlocked, 1
		if i == 0 {
			state, in = TaskReady, 0
		}
		c.State, c.InDegree, c.IsCompensation = state, in, true
		var req any
		if len(c.Request) > 0 {
			req = c.Request
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ne_task (tenant, id, order_id, ne_tenant, ne_id, domain, operation, state, in_degree,
				priority, seq, is_compensation, compensates_id, idempotency_key, plan, request, dispatched_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,true,$12,$13,$14,$15, CASE WHEN $8='READY' THEN now() END)`,
			c.Tenant, c.ID, c.OrderID, c.NETenant, c.NEID, c.Domain, c.Operation, state, in, int(c.Priority), c.Seq,
			c.CompensatesID, c.IdempotencyKey, c.Plan, req); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO bc_task_link (tenant, order_id, bc_id, task_id) VALUES ($1,$2,$3,$4)`,
			c.Tenant, c.OrderID, bcID, c.ID); err != nil {
			return nil, err
		}
		if prev != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO task_dependency (tenant, order_id, task_id, depends_on_id, kind)
				VALUES ($1,$2,$3,$4,'compensation')`, c.Tenant, c.OrderID, c.ID, prev); err != nil {
				return nil, err
			}
		}
		prev = c.ID
	}
	if len(comps) == 0 {
		return nil, nil
	}
	return comps[0], nil
}

// MarkCompensated sets the forward task state after its compensation succeeded.
func MarkCompensated(ctx context.Context, tx pgx.Tx, tenantID, taskID string) error {
	_, err := tx.Exec(ctx, `UPDATE ne_task SET state='COMPENSATED', updated_at=now() WHERE tenant=$1 AND id=$2`, tenantID, taskID)
	return err
}

// SetOrderState updates the order state (completed_at for terminal states) and returns the external id.
func SetOrderState(ctx context.Context, tx pgx.Tx, tenantID, orderID, state string, terminal bool, code, msg string) (string, error) {
	var ext string
	err := tx.QueryRow(ctx, `UPDATE service_order SET state=$3, updated_at=now(), version=version+1,
			completed_at = CASE WHEN $4 THEN now() ELSE completed_at END,
			error_code = coalesce($5, error_code), error = coalesce($6, error)
		WHERE tenant=$1 AND id=$2 RETURNING coalesce(external_id,'')`, tenantID, orderID, state, terminal,
		nullable(code), nullable(msg)).Scan(&ext)
	return ext, err
}
