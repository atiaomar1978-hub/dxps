// Package orchestrator implements the DxPS orchestration core (LLD 6.6 - 6.8): BusinessCommand
// ingestion (inbox dedupe, entity admission, planning), task result handling (DAG progression,
// retry ladder, saga compensation) and TMF641/TMF688 state propagation through the outbox.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"dxps/internal/catalog"
	"dxps/internal/contract"
	"dxps/internal/ids"
	"dxps/internal/planner"
	"dxps/internal/registry"
	"dxps/internal/saga"
	"dxps/internal/store"
	"dxps/internal/tenant"
)

// Publisher fast-publishes committed outbox rows (bus.FastPublish in production).
type Publisher interface {
	Publish(ctx context.Context, rows []*store.OutboxRow) error
}

type Orchestrator struct {
	Store       *store.Store
	Catalog     *catalog.Catalog
	Planner     *planner.Planner
	Tenants     *tenant.Registry
	Registry    *registry.Registry
	Pub         Publisher
	Log         *slog.Logger
	MaxAttempts int
	Now         func() time.Time
}

func (o *Orchestrator) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *Orchestrator) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

// txc carries the transaction and the outbox rows to publish after commit.
type txc struct {
	ctx    context.Context
	tx     pgx.Tx
	tenant string
	out    []*store.OutboxRow
}

func (o *Orchestrator) run(ctx context.Context, tenantID string, fn func(*txc) error) error {
	var c *txc
	err := o.Store.InTenant(ctx, tenantID, func(tx pgx.Tx) error {
		c = &txc{ctx: ctx, tx: tx, tenant: tenantID}
		if err := fn(c); err != nil {
			return err
		}
		return store.InsertOutbox(ctx, tx, c.out)
	})
	if err != nil {
		return err
	}
	if o.Pub != nil && len(c.out) > 0 {
		if perr := o.Pub.Publish(ctx, c.out); perr != nil {
			o.log().Warn("fast publish failed; relay will publish", "err", perr, "rows", len(c.out))
		}
	}
	return nil
}

var codeRe = regexp.MustCompile(`^(DXPS-[0-9]{4})`)

func errCode(err error, def string) string {
	if m := codeRe.FindStringSubmatch(err.Error()); m != nil {
		return m[1]
	}
	return def
}

// ErrPermanent wraps errors that must not be retried by the consumer (poison messages -> DLQ).
var ErrPermanent = errors.New("permanent")

// ---------------------------------------------------------------- BusinessCommand ingestion

// HandleBC processes one BusinessCommand. headerTenant is the Kafka record's tenant header.
func (o *Orchestrator) HandleBC(ctx context.Context, bc *contract.BusinessCommand, headerTenant, key string) error {
	if err := bc.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	if err := contract.CheckTenant(headerTenant, key, bc.Tenant); err != nil {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	_, terr := o.Tenants.Require(bc.Tenant)
	spec, serr := o.Catalog.Resolve(bc.Tenant, bc.CommandSpec, bc.SpecVersion)
	if serr == nil {
		serr = spec.Validate(bc.Params)
	}
	return o.run(ctx, bc.Tenant, func(c *txc) error {
		dup, err := store.InboxInsert(ctx, c.tx, bc.Tenant, bc.MessageID)
		if err != nil || dup {
			return err
		}
		if err := store.EnsureOrder(ctx, c.tx, bc); err != nil {
			return err
		}
		state, err := store.LockOrder(ctx, c.tx, bc.Tenant, bc.OrderID)
		if err != nil {
			return err
		}
		if saga.Terminal(state) {
			return nil // cancelled / rejected / finished order: ignore late commands
		}
		if terr != nil || serr != nil {
			cause := terr
			if cause == nil {
				cause = serr
			}
			return o.setOrder(c, bc.OrderID, saga.Rejected, errCode(cause, "DXPS-1002"), cause.Error())
		}
		b := &store.BC{Tenant: bc.Tenant, ID: bc.BCID, OrderID: bc.OrderID, ItemID: bc.OrderItemID, MessageID: bc.MessageID,
			EntityKeys: bc.EntityKeys(), EntitySeq: bc.EntitySeq, SpecID: spec.ID, SpecTenant: spec.Tenant,
			SpecCode: spec.Code, SpecVer: spec.Version, Action: bc.Action, Priority: bc.Priority, TxMode: bc.TxMode,
			Params: bc.Params, DependsOnItems: bc.DependsOnItems, Preempt: spec.Preempt}
		inserted, err := store.InsertBC(ctx, c.tx, b)
		if err != nil || !inserted {
			return err
		}
		if err := o.tryStart(c, b); err != nil {
			return err
		}
		return o.updateOrder(c, bc.OrderID)
	})
}

// tryStart admits and plans a pending BC if its item dependencies and entity sequencer allow it.
func (o *Orchestrator) tryStart(c *txc, b *store.BC) error {
	ready, failed, err := store.ItemDependency(c.ctx, c.tx, b)
	if err != nil {
		return err
	}
	if failed {
		return o.finishBC(c, b, store.BCFailed, "DXPS-2002", "order item dependency did not complete")
	}
	if !ready {
		return nil
	}
	adm, err := store.Admit(c.ctx, c.tx, b.Tenant, b.EntityKeys, b.ID, b.EntitySeq)
	if err != nil {
		return err
	}
	switch adm {
	case store.Parked:
		return nil
	case store.Stale:
		return o.finishBC(c, b, store.BCRejected, "DXPS-2003", "stale entity sequence")
	}
	spec, err := o.Catalog.Resolve(b.Tenant, b.SpecCode, b.SpecVer)
	if err != nil {
		return o.finishBC(c, b, store.BCRejected, errCode(err, "DXPS-1001"), err.Error())
	}
	existing, err := store.CoalesceCandidates(c.ctx, c.tx, b.Tenant, b.OrderID)
	if err != nil {
		return err
	}
	plan, err := o.Planner.Build(spec, b.ToCommand(), existing)
	if err != nil {
		return o.finishBC(c, b, store.BCFailed, errCode(err, "DXPS-1003"), err.Error())
	}
	if len(plan.Tasks) == 0 {
		return o.finishBC(c, b, store.BCCompleted, "", "no task applies")
	}
	readyTasks, err := store.InsertPlan(c.ctx, c.tx, b.ID, plan)
	if err != nil {
		return err
	}
	if err := store.SetBCState(c.ctx, c.tx, b.Tenant, b.ID, store.BCInProgress, "", ""); err != nil {
		return err
	}
	b.State = store.BCInProgress
	for _, t := range readyTasks {
		o.dispatch(c, t, 0)
	}
	return nil // a fully coalesced BC completes with the tasks it was merged into (bc_task_link)
}

// dispatch queues a task (or its retry) in the outbox. delay > 0 routes through the retry ladder.
func (o *Orchestrator) dispatch(c *txc, t *store.Task, delay time.Duration) {
	msg := &contract.NeTask{
		Tenant: t.Tenant, TaskID: t.ID, OrderID: t.OrderID, NETenant: t.NETenant, NEID: t.NEID, NECode: t.Plan.NECode,
		Domain: t.Domain, EntityKey: t.Plan.EntityKey, Operation: t.Operation, Request: t.Request, Params: t.Plan.Params,
		SubscriberGroup: t.Plan.SubscriberGroup, IdempotencyKey: t.IdempotencyKey, Attempt: t.Attempt,
		Priority: t.Priority, Compensation: t.IsCompensation,
	}
	if len(msg.Request) == 0 {
		msg.Request = json.RawMessage("{}")
	}
	payload, _ := json.Marshal(msg)
	topic := contract.TaskTopic(t.Domain, t.Priority)
	h := map[string]string{
		contract.HdrTenant: t.Tenant, contract.HdrPriority: t.Priority.String(),
		contract.HdrMessageID: ids.Derive("task", t.ID, strconv.Itoa(t.Attempt)).String(), contract.HdrSchema: "dxps.v1.NeTask",
	}
	if delay > 0 {
		h[contract.HdrTarget] = topic
		h[contract.HdrNotBefore] = o.now().Add(delay).UTC().Format(time.RFC3339Nano)
		topic = contract.RetryTopicFor(delay)
	}
	c.out = append(c.out, &store.OutboxRow{Tenant: t.Tenant, Topic: topic,
		Key: contract.Key(t.Tenant, t.NEID, t.Plan.EntityKey), Payload: payload, Headers: h})
}

// ---------------------------------------------------------------- results

// HandleResult applies an adapter TaskResult (LLD 6.8).
func (o *Orchestrator) HandleResult(ctx context.Context, r *contract.TaskResult, headerTenant, key string) error {
	if err := contract.CheckTenant(headerTenant, key, r.Tenant); err != nil {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	if !ids.ValidUUID(r.TaskID) || !ids.ValidUUID(r.OrderID) {
		return fmt.Errorf("%w: invalid ids", ErrPermanent)
	}
	return o.run(ctx, r.Tenant, func(c *txc) error {
		msgID := ids.Derive("result", r.TaskID, strconv.Itoa(r.Attempt), string(r.Outcome)).String()
		dup, err := store.InboxInsert(ctx, c.tx, r.Tenant, msgID)
		if err != nil || dup {
			return err
		}
		if _, err := store.LockOrder(ctx, c.tx, r.Tenant, r.OrderID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}
		t, err := store.GetTask(ctx, c.tx, r.Tenant, r.TaskID, true)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if t.OrderID != r.OrderID || t.Terminal() || t.Attempt != r.Attempt || t.State != store.TaskReady {
			return nil // stale or duplicate result
		}
		if err := store.RecordAttempt(ctx, c.tx, t, r); err != nil {
			return err
		}
		t.NEStatus, t.NECode, t.Response = r.NEStatus, r.NECode, r.Response
		outcome := r.Outcome
		if outcome == contract.Retryable && saga.Exhausted(t.Attempt, o.MaxAttempts, nil, o.now()) {
			outcome, t.NECode = contract.Failed, "DXPS-4001"
		}
		switch outcome {
		case contract.Succeeded:
			t.State, t.LastError = store.TaskSucceeded, ""
			if err := store.UpdateTask(ctx, c.tx, t); err != nil {
				return err
			}
			if t.IsCompensation {
				if err := store.MarkCompensated(ctx, c.tx, t.Tenant, t.CompensatesID); err != nil {
					return err
				}
			}
			if err := o.unblock(c, t); err != nil {
				return err
			}
		case contract.Retryable:
			t.Attempt++
			t.LastError = r.Message
			if err := store.UpdateTask(ctx, c.tx, t); err != nil {
				return err
			}
			o.dispatch(c, t, saga.NextDelay(t.Attempt, r.RetryAfter, nil))
			return nil
		default:
			t.State = store.TaskFailed
			t.LastError = r.Message
			if t.NECode == "" {
				t.NECode = fmt.Sprintf("DXPS-3%03d", r.NEStatus%1000)
			}
			if err := store.UpdateTask(ctx, c.tx, t); err != nil {
				return err
			}
			if err := store.SkipDependants(ctx, c.tx, t.Tenant, t.ID); err != nil {
				return err
			}
			c.out = append(c.out, o.dlq(t, r))
		}
		bcs, err := store.BCsOfTask(ctx, c.tx, t.Tenant, t.ID)
		if err != nil {
			return err
		}
		for _, id := range bcs {
			b, err := store.GetBC(ctx, c.tx, t.Tenant, id, true)
			if err != nil {
				return err
			}
			if err := o.evaluateBC(c, b, t); err != nil {
				return err
			}
		}
		return o.updateOrder(c, t.OrderID)
	})
}

func (o *Orchestrator) dlq(t *store.Task, r *contract.TaskResult) *store.OutboxRow {
	p, _ := json.Marshal(map[string]any{"task": t.ID, "order": t.OrderID, "operation": t.Operation, "ne": t.Plan.NECode,
		"attempt": t.Attempt, "neStatus": r.NEStatus, "code": t.NECode, "message": r.Message})
	return &store.OutboxRow{Tenant: t.Tenant, Topic: contract.TopicDLQ, Key: contract.Key(t.Tenant, t.OrderID), Payload: p,
		Headers: map[string]string{contract.HdrTenant: t.Tenant, contract.HdrPriority: "p2"}}
}

// unblock renders and dispatches dependants of t that became ready.
func (o *Orchestrator) unblock(c *txc, t *store.Task) error {
	deps, err := store.UnblockDependants(c.ctx, c.tx, t.Tenant, t.ID)
	if err != nil {
		return err
	}
	for _, d := range deps {
		if d.InDegree > 0 {
			continue
		}
		res, err := store.DependencyResults(c.ctx, c.tx, d.Tenant, d.ID, d.CompensatesID)
		if err != nil {
			return err
		}
		if d.IsCompensation {
			d.Request, err = o.Planner.RenderCompensation(d.ID, &d.Plan, res)
		} else {
			d.Request, err = o.Planner.Render(d.ID, &d.Plan, res)
		}
		if err != nil {
			d.State, d.NECode, d.LastError = store.TaskFailed, "DXPS-1002", err.Error()
			if err := store.UpdateTask(c.ctx, c.tx, d); err != nil {
				return err
			}
			continue
		}
		d.State = store.TaskReady
		if err := store.UpdateTask(c.ctx, c.tx, d); err != nil {
			return err
		}
		o.dispatch(c, d, 0)
	}
	return nil
}

// evaluateBC advances the saga of one business command after a task changed state.
func (o *Orchestrator) evaluateBC(c *txc, b *store.BC, changed *store.Task) error {
	if store.BCTerminal(b.State) {
		return nil
	}
	tasks, deps, err := store.TasksOfBC(c.ctx, c.tx, b.Tenant, b.ID)
	if err != nil {
		return err
	}
	var fwd, comp []*store.Task
	for _, t := range tasks {
		if t.IsCompensation {
			comp = append(comp, t)
		} else {
			fwd = append(fwd, t)
		}
	}
	inflight, failedFwd, allOK := false, (*store.Task)(nil), true
	for _, t := range fwd {
		switch t.State {
		case store.TaskReady, store.TaskBlocked:
			inflight, allOK = true, false
		case store.TaskFailed:
			allOK = false
			if failedFwd == nil {
				failedFwd = t
			}
		case store.TaskSkipped:
			allOK = false
		}
	}
	if b.State == store.BCCompensating {
		done := true
		for _, t := range comp {
			switch t.State {
			case store.TaskFailed:
				return o.finishBC(c, b, store.BCFailed, "DXPS-5001", "compensation failed: "+t.Operation)
			case store.TaskSucceeded:
			default:
				done = false
			}
		}
		if len(comp) == 0 && !inflight {
			return o.compensate(c, b, fwd, deps)
		}
		if done && !inflight {
			return o.finishBC(c, b, store.BCFailed, codeOf(failedFwd), "rolled back after task failure")
		}
		return nil
	}
	if failedFwd == nil {
		if allOK {
			return o.finishBC(c, b, store.BCCompleted, "", "")
		}
		return nil
	}
	if b.TxMode == contract.Atomic {
		if err := store.SkipBlocked(c.ctx, c.tx, b.Tenant, b.ID); err != nil {
			return err
		}
		if err := store.SetBCState(c.ctx, c.tx, b.Tenant, b.ID, store.BCCompensating, failedFwd.NECode, failedFwd.LastError); err != nil {
			return err
		}
		b.State = store.BCCompensating
		if inflight {
			// re-read states: tasks blocked a moment ago are now skipped
			for _, t := range fwd {
				if t.State == store.TaskReady {
					return nil // wait for in-flight siblings before compensating
				}
			}
		}
		return o.compensate(c, b, fwd, deps)
	}
	if inflight {
		for _, t := range fwd {
			if t.State == store.TaskReady {
				return nil
			}
		}
	}
	return o.finishBC(c, b, store.BCFailed, failedFwd.NECode, failedFwd.LastError)
}

func codeOf(t *store.Task) string {
	if t != nil && t.NECode != "" {
		return t.NECode
	}
	return "DXPS-3000"
}

// compensate creates the reverse-order compensation chain for an ATOMIC BC.
func (o *Orchestrator) compensate(c *txc, b *store.BC, fwd []*store.Task, deps map[string][]string) error {
	var failed *store.Task
	nodes := make([]saga.Node, 0, len(fwd))
	byID := map[string]*store.Task{}
	for _, t := range fwd {
		byID[t.ID] = t
		if t.State == store.TaskFailed && failed == nil {
			failed = t
		}
		nodes = append(nodes, saga.Node{ID: t.ID, DependsOn: deps[t.ID], Seq: t.Seq,
			Succeeded: t.State == store.TaskSucceeded, Pivot: t.AfterPivot, HasComp: t.Plan.CompOp != ""})
	}
	order := saga.CompensationOrder(nodes)
	code, msg := "DXPS-3000", "task failed"
	if failed != nil {
		code, msg = failed.NECode, failed.LastError
	}
	if len(order) == 0 {
		return o.finishBC(c, b, store.BCFailed, code, msg)
	}
	comps := make([]*store.Task, 0, len(order))
	for i, id := range order {
		f := byID[id]
		cid := ids.NewString()
		ct := &store.Task{Tenant: f.Tenant, ID: cid, OrderID: f.OrderID, NETenant: f.NETenant, NEID: f.NEID, Domain: f.Domain,
			Operation: f.Plan.CompOp, Priority: f.Priority, Seq: 1000 + i, CompensatesID: f.ID,
			IdempotencyKey: f.Tenant + ":" + cid + ":0", Plan: f.Plan}
		if i == 0 {
			res, err := store.DependencyResults(c.ctx, c.tx, f.Tenant, cid, f.ID)
			if err != nil {
				return err
			}
			if ct.Request, err = o.Planner.RenderCompensation(cid, &ct.Plan, res); err != nil {
				return o.finishBC(c, b, store.BCFailed, "DXPS-5001", err.Error())
			}
		}
		comps = append(comps, ct)
	}
	first, err := store.InsertCompensations(c.ctx, c.tx, b.ID, comps)
	if err != nil {
		return err
	}
	o.dispatch(c, first, 0)
	return nil
}

// finishBC moves a BC to a terminal state, releases its entities and admits waiting commands.
func (o *Orchestrator) finishBC(c *txc, b *store.BC, state, code, msg string) error {
	if err := store.SetBCState(c.ctx, c.tx, b.Tenant, b.ID, state, code, msg); err != nil {
		return err
	}
	b.State = state
	if err := store.Release(c.ctx, c.tx, b.Tenant, b.EntityKeys, b.ID); err != nil {
		return err
	}
	parked, err := store.ParkedFor(c.ctx, c.tx, b.Tenant, b.EntityKeys)
	if err != nil {
		return err
	}
	waiting, err := store.WaitingOnItems(c.ctx, c.tx, b.Tenant, b.OrderID)
	if err != nil {
		return err
	}
	seen := map[string]bool{b.ID: true}
	for _, n := range append(parked, waiting...) {
		if seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		cur, err := store.GetBC(c.ctx, c.tx, n.Tenant, n.ID, true)
		if err != nil {
			return err
		}
		if cur.State != store.BCPending {
			continue
		}
		if err := o.tryStart(c, cur); err != nil {
			return err
		}
		if cur.OrderID != b.OrderID {
			if err := o.updateOrder(c, cur.OrderID); err != nil {
				return err
			}
		}
	}
	return nil
}

// updateOrder derives the order state from its BCs and emits TMF688 / state events on change.
func (o *Orchestrator) updateOrder(c *txc, orderID string) error {
	cur, err := store.LockOrder(c.ctx, c.tx, c.tenant, orderID)
	if err != nil {
		return err
	}
	if saga.Terminal(cur) {
		return nil
	}
	bcs, err := store.OrderBCs(c.ctx, c.tx, c.tenant, orderID)
	if err != nil {
		return err
	}
	states := make([]string, len(bcs))
	var code, msg string
	for i, b := range bcs {
		states[i] = b.State
	}
	next := saga.OrderState(states)
	if next == saga.InProgress {
		allPending := true
		for _, s := range states {
			if s != store.BCPending {
				allPending = false
			}
		}
		if allPending {
			next = saga.Pending
		}
	}
	if next == cur {
		return nil
	}
	if next == saga.Failed || next == saga.Partial {
		code, msg = "DXPS-3000", "one or more order items failed"
	}
	return o.setOrder(c, orderID, next, code, msg)
}

func (o *Orchestrator) setOrder(c *txc, orderID, state, code, msg string) error {
	ext, err := store.SetOrderState(c.ctx, c.tx, c.tenant, orderID, state, saga.Terminal(state), code, msg)
	if err != nil {
		return err
	}
	now := o.now().UTC()
	ev := contract.OrderEvent{EventID: ids.NewString(), EventTime: now, EventType: "ServiceOrderStateChangeEvent"}
	ev.Event.ServiceOrder = contract.OrderRef{ID: orderID, ExternalID: ext, State: state}
	if saga.Terminal(state) {
		ev.Event.ServiceOrder.CompletionDate = &now
	}
	p, _ := json.Marshal(ev)
	h := map[string]string{contract.HdrTenant: c.tenant, contract.HdrPriority: "p1", contract.HdrMessageID: ev.EventID,
		contract.HdrSchema: "tmf688.ServiceOrderStateChangeEvent"}
	k := contract.Key(c.tenant, orderID)
	c.out = append(c.out,
		&store.OutboxRow{Tenant: c.tenant, Topic: contract.TopicEvent, Key: k, Payload: p, Headers: h},
		&store.OutboxRow{Tenant: c.tenant, Topic: contract.TopicStateOrder, Key: k, Payload: p, Headers: h})
	return nil
}
