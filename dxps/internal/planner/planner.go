// Package planner decomposes a BusinessCommand into a DAG of NE tasks using the tenant-resolved
// catalog spec, NE selection with tenant entitlement and coalescing of tasks on the same resource.
package planner

import (
	"encoding/json"
	"fmt"

	"dxps/internal/catalog"
	"dxps/internal/contract"
	"dxps/internal/ids"
	"dxps/internal/registry"
)

// TaskPlan is the persisted planning context of a task (ne_task.plan); it allows rendering the
// request when the task becomes ready, with the responses of the tasks it depends on.
type TaskPlan struct {
	Tenant          string         `json:"tenant"`
	SpecTenant      string         `json:"specTenant"`
	Spec            string         `json:"spec"`
	SpecVersion     string         `json:"specVersion"`
	SpecTask        string         `json:"specTask"`
	Domain          string         `json:"domain"`
	Template        string         `json:"template,omitempty"`
	CompOp          string         `json:"compOp,omitempty"`
	CompTemplate    string         `json:"compTemplate,omitempty"`
	Params          map[string]any `json:"params"`
	NECode          string         `json:"neCode"`
	SubscriberGroup string         `json:"subscriberGroup,omitempty"`
	DependsOnSpec   []string       `json:"dependsOnSpec,omitempty"`
	EntityKey       string         `json:"entityKey"`
}

type Task struct {
	ID             string
	Seq            int
	NE             *registry.NE
	Op             string
	Priority       contract.Priority
	CoalesceKey    string
	Pivot          bool
	IdempotencyKey string
	DependsOn      []string // task ids
	Request        json.RawMessage
	Plan           TaskPlan
	MergedInto     string // existing (not yet dispatched) task this one was coalesced into
}

func (t *Task) Ready() bool { return len(t.DependsOn) == 0 && t.MergedInto == "" }

type Plan struct {
	BC    *contract.BusinessCommand
	Spec  *catalog.Spec
	Tasks []*Task
}

// New returns the tasks to create (excluding coalesced ones).
func (p *Plan) New() []*Task {
	var out []*Task
	for _, t := range p.Tasks {
		if t.MergedInto == "" {
			out = append(out, t)
		}
	}
	return out
}

type Planner struct {
	Catalog  *catalog.Catalog
	Registry *registry.Registry
}

// Build plans bc. existing maps coalesce keys of not-yet-dispatched tasks in the same order to task ids.
func (pl *Planner) Build(spec *catalog.Spec, bc *contract.BusinessCommand, existing map[string]string) (*Plan, error) {
	plan := &Plan{BC: bc, Spec: spec}
	bySpec := map[string]*Task{}
	effDeps := map[string][]string{} // spec task id -> included spec task ids it depends on
	for i, ts := range spec.Tasks {
		var deps []string
		for _, d := range ts.DependsOn {
			if _, ok := bySpec[d]; ok {
				deps = append(deps, d)
			} else {
				deps = append(deps, effDeps[d]...) // dependency skipped: inherit its dependencies
			}
		}
		deps = dedupe(deps)
		effDeps[ts.ID] = deps
		ok, err := ts.Applies(bc.Tenant, bc.Params)
		if err != nil {
			return nil, fmt.Errorf("%w: when: %v", catalog.ErrParams, err)
		}
		if !ok {
			continue
		}
		sel, err := pl.Registry.Select(bc.Tenant, ts.NE.NFType, ts.Op)
		if err != nil {
			return nil, err
		}
		ck, err := ts.CoalesceKeyFor(bc.Tenant, bc.Params)
		if err != nil {
			return nil, fmt.Errorf("%w: coalesceKey: %v", catalog.ErrParams, err)
		}
		id := ids.NewString()
		t := &Task{
			ID: id, Seq: i, NE: sel.NE, Op: ts.Op, Priority: bc.Priority, CoalesceKey: ck, Pivot: ts.Pivot,
			IdempotencyKey: bc.Tenant + ":" + id + ":0",
			Plan: TaskPlan{
				Tenant: bc.Tenant, SpecTenant: spec.Tenant, Spec: spec.Code, SpecVersion: spec.Version, SpecTask: ts.ID,
				Domain: ts.Domain, Template: ts.Template, CompOp: ts.Compensate, CompTemplate: ts.CompensateTemplate,
				Params: bc.Params, NECode: sel.NE.Code, SubscriberGroup: sel.Access.SubscriberGroup,
				DependsOnSpec: deps, EntityKey: bc.EntityKey,
			},
		}
		if ck != "" {
			if ex, ok := existing[ck]; ok {
				t.MergedInto = ex
			}
		}
		for _, d := range deps {
			if dt := bySpec[d]; dt.MergedInto == "" {
				t.DependsOn = append(t.DependsOn, dt.ID)
			}
		}
		if t.Ready() {
			if t.Request, err = pl.Render(t.ID, &t.Plan, nil); err != nil {
				return nil, err
			}
		}
		bySpec[ts.ID] = t
		plan.Tasks = append(plan.Tasks, t)
	}
	return plan, nil // may be empty when no task applies (noChange)
}

// Render renders the forward request of a task with the responses of completed dependencies.
func (pl *Planner) Render(taskID string, tp *TaskPlan, results map[string]map[string]any) (json.RawMessage, error) {
	return pl.Catalog.Render(tp.Template, pl.data(taskID, tp, results))
}

// RenderCompensation renders the compensation request (empty JSON object when no template).
func (pl *Planner) RenderCompensation(taskID string, tp *TaskPlan, results map[string]map[string]any) (json.RawMessage, error) {
	return pl.Catalog.Render(tp.CompTemplate, pl.data(taskID, tp, results))
}

func (pl *Planner) data(taskID string, tp *TaskPlan, results map[string]map[string]any) catalog.RenderData {
	return catalog.RenderData{
		Tenant: tp.Tenant, TaskID: taskID, Params: tp.Params, NECode: tp.NECode,
		SubscriberGroup: tp.SubscriberGroup, Results: results,
	}
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	out := xs[:0:0]
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
