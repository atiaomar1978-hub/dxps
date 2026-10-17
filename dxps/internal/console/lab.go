package console

import (
	"encoding/json"
	"errors"
	"sort"

	"dxps/internal/catalog"
	"dxps/internal/registry"
)

// LabRequest is a dry run of one order item: no message is published and no NE is called.
type LabRequest struct {
	Tenant  string                    `json:"tenant"`
	Spec    string                    `json:"spec"`
	Version string                    `json:"version,omitempty"`
	Params  map[string]any            `json:"params"`
	Draft   string                    `json:"draft,omitempty"`
	Results map[string]map[string]any `json:"results,omitempty"` // spec task id -> simulated NE response
}

type LabTask struct {
	ID              string          `json:"id"`
	Wave            int             `json:"wave"`
	Op              string          `json:"op"`
	Domain          string          `json:"domain"`
	NFType          string          `json:"nfType"`
	When            string          `json:"when,omitempty"`
	Applies         bool            `json:"applies"`
	WhenError       string          `json:"whenError,omitempty"`
	DependsOn       []string        `json:"dependsOn"`
	NE              string          `json:"ne,omitempty"`
	NETenant        string          `json:"neTenant,omitempty"`
	Shared          bool            `json:"shared,omitempty"`
	SubscriberGroup string          `json:"subscriberGroup,omitempty"`
	NEError         string          `json:"neError,omitempty"`
	CoalesceKey     string          `json:"coalesceKey,omitempty"`
	Pivot           bool            `json:"pivot,omitempty"`
	Template        string          `json:"template,omitempty"`
	Request         json.RawMessage `json:"request,omitempty"`
	RenderError     string          `json:"renderError,omitempty"`
	Compensate      string          `json:"compensate,omitempty"`
	CompTemplate    string          `json:"compensateTemplate,omitempty"`
	CompRequest     json.RawMessage `json:"compensateRequest,omitempty"`
	CompError       string          `json:"compensateError,omitempty"`
}

type LabResult struct {
	Tenant     string          `json:"tenant"`
	SpecTenant string          `json:"specTenant,omitempty"`
	Spec       string          `json:"spec"`
	Version    string          `json:"version,omitempty"`
	TxMode     string          `json:"txMode,omitempty"`
	Priority   string          `json:"priority,omitempty"`
	Valid      bool            `json:"valid"`
	Problems   []string        `json:"problems"`
	EntityKeys []string        `json:"entityKeys"`
	Tasks      []LabTask       `json:"tasks"`
	Order      json.RawMessage `json:"order,omitempty"`
}

// RunLab resolves the spec for the tenant (tenant override first, then GLOBAL), validates the parameters,
// evaluates entity keys, "when" conditions and coalesce keys, selects NEs with the tenant's entitlements and
// renders forward and compensation payloads. Tasks that depend on others render with Results (if given).
func RunLab(cat *catalog.Catalog, reg *registry.Registry, q LabRequest) *LabResult {
	res := &LabResult{Tenant: q.Tenant, Spec: q.Spec, Problems: []string{}, EntityKeys: []string{}, Tasks: []LabTask{}}
	if q.Params == nil {
		q.Params = map[string]any{}
	}
	spec, err := cat.Resolve(q.Tenant, q.Spec, q.Version)
	if err != nil {
		res.Problems = append(res.Problems, err.Error())
		return res
	}
	res.SpecTenant, res.Version, res.TxMode, res.Priority = spec.Tenant, spec.Version, string(spec.TxMode), spec.Priority().String()
	if err := spec.Validate(q.Params); err != nil {
		var pe *catalog.ParamError
		if errors.As(err, &pe) {
			res.Problems = append(res.Problems, pe.Problems...)
		} else {
			res.Problems = append(res.Problems, err.Error())
		}
	}
	if len(res.Problems) == 0 {
		if keys, err := spec.EntityKeysFor(q.Tenant, q.Params); err != nil {
			res.Problems = append(res.Problems, err.Error())
		} else {
			res.EntityKeys = keys
		}
	}
	wave := map[string]int{}
	included := map[string]bool{}
	effDeps := map[string][]string{}
	for _, ts := range spec.Tasks {
		lt := LabTask{ID: ts.ID, Op: ts.Op, Domain: ts.Domain, NFType: ts.NE.NFType, When: ts.When, Pivot: ts.Pivot,
			Template: ts.Template, Compensate: ts.Compensate, CompTemplate: ts.CompensateTemplate, DependsOn: []string{}}
		var deps []string
		for _, d := range ts.DependsOn {
			if included[d] {
				deps = append(deps, d)
			} else {
				deps = append(deps, effDeps[d]...)
			}
		}
		deps = uniq(deps)
		effDeps[ts.ID] = deps
		lt.DependsOn = append(lt.DependsOn, deps...)
		for _, d := range deps {
			lt.Wave = max(lt.Wave, wave[d]+1)
		}
		ok, err := ts.Applies(q.Tenant, q.Params)
		if err != nil {
			lt.WhenError = err.Error()
		}
		lt.Applies = ok && err == nil
		if !lt.Applies {
			res.Tasks = append(res.Tasks, lt)
			continue
		}
		included[ts.ID] = true
		wave[ts.ID] = lt.Wave
		sel, err := reg.Select(q.Tenant, ts.NE.NFType, ts.Op)
		if err != nil {
			lt.NEError = err.Error()
		} else {
			lt.NE, lt.NETenant, lt.Shared, lt.SubscriberGroup = sel.NE.Code, sel.NE.Tenant, sel.NE.Shared, sel.Access.SubscriberGroup
		}
		if ck, err := ts.CoalesceKeyFor(q.Tenant, q.Params); err != nil {
			lt.RenderError = "coalesce key: " + err.Error()
		} else {
			lt.CoalesceKey = ck
		}
		data := catalog.RenderData{Tenant: q.Tenant, TaskID: "lab-" + ts.ID, Params: q.Params, NECode: lt.NE,
			SubscriberGroup: lt.SubscriberGroup, Results: q.Results}
		if b, err := cat.Render(ts.Template, data); err != nil {
			lt.RenderError = err.Error()
		} else {
			lt.Request = b
		}
		if ts.CompensateTemplate != "" {
			if b, err := cat.Render(ts.CompensateTemplate, data); err != nil {
				lt.CompError = err.Error()
			} else {
				lt.CompRequest = b
			}
		}
		res.Tasks = append(res.Tasks, lt)
	}
	res.Valid = len(res.Problems) == 0
	for _, t := range res.Tasks {
		if t.Applies && (t.NEError != "" || t.WhenError != "") {
			res.Valid = false
		}
	}
	res.Order = SampleOrder("LAB-"+q.Spec, []OrderItemReq{{ID: "item1", Spec: q.Spec, Params: q.Params}}, "")
	return res
}

func uniq(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// OrderItemReq is one item of an order composed in the console.
type OrderItemReq struct {
	ID        string         `json:"id"`
	Spec      string         `json:"spec"`
	Action    string         `json:"action,omitempty"`
	Params    map[string]any `json:"params"`
	DependsOn []string       `json:"dependsOn,omitempty"`
}

// SampleOrder builds a TMF641 ServiceOrder body for the gateway.
func SampleOrder(externalID string, items []OrderItemReq, priority string) json.RawMessage {
	out := make([]any, 0, len(items))
	for _, it := range items {
		keys := make([]string, 0, len(it.Params))
		for k := range it.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		chars := make([]any, 0, len(keys))
		for _, k := range keys {
			chars = append(chars, map[string]any{"name": k, "value": it.Params[k]})
		}
		action := it.Action
		if action == "" {
			action = "add"
		}
		m := map[string]any{"id": it.ID, "action": action, "@type": "ServiceOrderItem",
			"service": map[string]any{"@type": "Service", "serviceSpecification": map[string]any{"id": it.Spec}, "serviceCharacteristic": chars}}
		if len(it.DependsOn) > 0 {
			var rels []any
			for _, d := range it.DependsOn {
				rels = append(rels, map[string]any{"relationshipType": "dependsOn", "orderItem": map[string]any{"itemId": d}})
			}
			m["serviceOrderItemRelationship"] = rels
		}
		out = append(out, m)
	}
	o := map[string]any{"@type": "ServiceOrder", "category": "console", "serviceOrderItem": out,
		"channel": []any{map[string]any{"name": "CONSOLE", "role": "orderChannel"}}}
	if externalID != "" {
		o["externalId"] = externalID
	}
	if priority != "" {
		o["priority"] = priority
	}
	b, _ := json.Marshal(o)
	return b
}
