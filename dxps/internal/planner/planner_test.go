package planner

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dxps/internal/catalog"
	"dxps/internal/contract"
	"dxps/internal/registry"
	"dxps/internal/testenv"
)

func setup(t *testing.T) *Planner {
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	return &Planner{Catalog: cat, Registry: testenv.Registry("localhost")}
}

func bc(tenant string, params map[string]any) *contract.BusinessCommand {
	return &contract.BusinessCommand{Tenant: tenant, OrderID: "o", BCID: "b", EntityKey: "supi:x", Priority: contract.P1, Params: params}
}

func bySpec(p *Plan) map[string]*Task {
	m := map[string]*Task{}
	for _, t := range p.Tasks {
		m[t.Plan.SpecTask] = t
	}
	return m
}

var sub = map[string]any{"supi": "imsi-416770000000001", "msisdn": "962790000001", "plan": "5G-100GB"}

// TC-PLN-001: CreateSubscriber5G decomposes into the UDR/OCS DAG; only roots are rendered at plan time.
func TestCreateSubscriberDAG(t *testing.T) {
	pl := setup(t)
	spec, _ := pl.Catalog.Resolve("host-mno", "CreateSubscriber5G", "")
	p, err := pl.Build(spec, bc("host-mno", sub), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := bySpec(p)
	if len(p.New()) != 5 || m["auth"].NE.Code != "udr01" || m["ocs"].NE.Code != "ocs01" || !m["ocs"].Pivot {
		t.Fatalf("plan %+v", m)
	}
	if !m["auth"].Ready() || m["auth"].Request == nil || m["am"].Request != nil || len(m["ocs"].DependsOn) != 2 {
		t.Fatal("rendering / dependencies")
	}
	if !strings.HasPrefix(m["auth"].IdempotencyKey, "host-mno:") || m["am"].CoalesceKey != "udr-am:imsi-416770000000001" {
		t.Fatal("idempotency / coalesce key")
	}
	var auth map[string]any
	_ = json.Unmarshal(m["auth"].Request, &auth)
	if auth["encPermanentKey"] != "hsm://host-mno/k/imsi-416770000000001" {
		t.Fatalf("key material must be an HSM reference: %v", auth["encPermanentKey"])
	}
}

// TC-PLN-002: A CEL `when` that is false skips the task; dependants inherit its dependencies.
func TestWhenSkip(t *testing.T) {
	pl := setup(t)
	spec, _ := pl.Catalog.Resolve("host-mno", "CreateSubscriber5G", "")
	p2 := map[string]any{"supi": "imsi-416770000000001", "msisdn": "962790000001", "plan": "LTE-20GB"}
	p, err := pl.Build(spec, bc("host-mno", p2), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bySpec(p)["policy"]; ok || len(p.Tasks) != 4 {
		t.Fatal("policy should be skipped for LTE plans")
	}
	del, _ := pl.Catalog.Resolve("host-mno", "DeleteSubscriber5G", "")
	if p, err = pl.Build(del, bc("host-mno", sub), nil); err != nil || len(bySpec(p)["auth"].DependsOn) != 3 {
		t.Fatalf("delete DAG %v", err)
	}
}

// TC-PLN-003: Tenant override spec and own NE are used for a FULL MVNO (mvno-alpha own OCS + welcome bundle).
func TestTenantOverride(t *testing.T) {
	pl := setup(t)
	spec, _ := pl.Catalog.Resolve("mvno-alpha", "CreateSubscriber5G", "")
	p, err := pl.Build(spec, bc("mvno-alpha", sub), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := bySpec(p)
	if spec.Version != "3.2.0-alpha.1" || m["ocs"].NE.Code != "ocs-alpha" || m["welcome"] == nil || m["am"].NE.Code != "udr01" {
		t.Fatalf("override not applied: %s", spec.Version)
	}
	if m["am"].Plan.SubscriberGroup != "mvno-alpha" {
		t.Fatal("subscriber group from entitlement missing")
	}
}

// TC-PLN-004: Planning fails with DXPS-1005 when the tenant is not entitled to the required NE.
func TestNotEntitled(t *testing.T) {
	pl := setup(t)
	spec, _ := pl.Catalog.Resolve("mvno-beta", "QoDSession", "")
	_, err := pl.Build(spec, bc("mvno-beta", map[string]any{"msisdn": "962790000001", "profile": "QOS_L", "durationSec": 60}), nil)
	if !errors.Is(err, registry.ErrNotEntitled) {
		t.Fatalf("got %v", err)
	}
}

// TC-PLN-005: Tasks on the same resource are coalesced into a not-yet-dispatched task of the same order.
func TestCoalesce(t *testing.T) {
	pl := setup(t)
	spec, _ := pl.Catalog.Resolve("host-mno", "ChangePlan", "")
	p, err := pl.Build(spec, bc("host-mno", sub), map[string]string{"udr-am:imsi-416770000000001": "existing-task"})
	if err != nil {
		t.Fatal(err)
	}
	m := bySpec(p)
	if m["am"].MergedInto != "existing-task" || m["am"].Ready() || len(p.New()) != 1 {
		t.Fatal("coalescing")
	}
}

// TC-PLN-006: Late rendering - eSIM ConfirmOrder uses the ICCID returned by DownloadOrder; compensation renders CancelOrder.
func TestLateRendering(t *testing.T) {
	pl := setup(t)
	spec, _ := pl.Catalog.Resolve("host-mno", "ProvisionESIM", "")
	p, err := pl.Build(spec, bc("host-mno", map[string]any{"eid": "89049032000001000000000000000123", "msisdn": "962790000001", "profileType": "5G"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := bySpec(p)
	if m["confirm"].Request != nil {
		t.Fatal("confirm rendered before download result")
	}
	if _, err := pl.Render("t", &m["confirm"].Plan, nil); err == nil {
		t.Fatal("render without dependency result must fail")
	}
	res := map[string]map[string]any{"download": {"iccid": "8949000000000000123"}}
	b, err := pl.Render("t", &m["confirm"].Plan, res)
	if err != nil || !strings.Contains(string(b), `"iccid":"8949000000000000123"`) {
		t.Fatalf("%s %v", b, err)
	}
	c, err := pl.RenderCompensation("t", &m["download"].Plan, res)
	if err != nil || !strings.Contains(string(c), "finalProfileStatusIndicator") {
		t.Fatalf("%s %v", c, err)
	}
	if e, _ := pl.RenderCompensation("t", &m["confirm"].Plan, nil); string(e) != "{}" {
		t.Fatalf("empty compensation template: %s", e)
	}
}

// TC-PLN-007: A spec whose CEL expressions fail at evaluation time is rejected with ErrParams.
func TestCELRuntimeErrors(t *testing.T) {
	pl := setup(t)
	spec, _ := pl.Catalog.Resolve("host-mno", "CreateSubscriber5G", "")
	// plan is not a string: startsWith fails at runtime
	_, err := pl.Build(spec, bc("host-mno", map[string]any{"supi": "imsi-416770000000001", "msisdn": "1", "plan": 5}), nil)
	if !errors.Is(err, catalog.ErrParams) {
		t.Fatalf("got %v", err)
	}
	cp, _ := pl.Catalog.Resolve("host-mno", "ChangePlan", "")
	if _, err := pl.Build(cp, bc("host-mno", map[string]any{"msisdn": "1", "plan": "x"}), nil); !errors.Is(err, catalog.ErrParams) {
		t.Fatalf("coalesce key error: %v", err)
	}
	empty := New(nil)
	if len(dedupe([]string{"a", "a", "b"})) != 2 || len(empty) != 0 {
		t.Fatal("dedupe")
	}
}

func New(ts []*Task) []*Task { return (&Plan{Tasks: ts}).New() }
