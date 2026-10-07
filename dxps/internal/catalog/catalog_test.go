package catalog

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

func mustLoad(t *testing.T) *Catalog {
	t.Helper()
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return c
}

// TC-CAT-001: Embedded catalog loads; every spec compiles (CEL), templates parse and examples validate and render to JSON.
func TestTC_CAT_001_EmbeddedCatalogValid(t *testing.T) {
	c := mustLoad(t)
	specs := c.All()
	if len(specs) < 10 {
		t.Fatalf("expected >= 10 specs, got %d", len(specs))
	}
	for _, s := range specs {
		if s.Example == nil {
			t.Errorf("%s/%s has no example", s.Tenant, s.Code)
			continue
		}
		params := normalize(s.Example)
		if err := s.Validate(params); err != nil {
			t.Errorf("%s example invalid: %v", s.Code, err)
		}
		keys, err := s.EntityKeysFor("host-mno", params)
		if err != nil || len(keys) == 0 {
			t.Errorf("%s entity keys: %v %v", s.Code, keys, err)
		}
		for _, ts := range s.Tasks {
			if _, err := ts.Applies("host-mno", params); err != nil {
				t.Errorf("%s/%s when: %v", s.Code, ts.ID, err)
			}
			res := map[string]map[string]any{"download": {"iccid": "8949000000000000001"}}
			for _, tpl := range []string{ts.Template, ts.CompensateTemplate} {
				out, err := c.Render(tpl, RenderData{Tenant: "host-mno", TaskID: "t1", Params: params, Results: res})
				if err != nil || !json.Valid(out) {
					t.Errorf("%s/%s render %s: %v", s.Code, ts.ID, tpl, err)
				}
			}
		}
	}
}

// normalize converts YAML example numbers to float64 as they arrive from JSON.
func normalize(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// TC-CAT-002: Tenant override resolution - mvno-alpha gets its own spec, other tenants get GLOBAL; unknown spec/version rejected (DXPS-1001).
func TestTC_CAT_002_TenantResolution(t *testing.T) {
	c := mustLoad(t)
	a, err := c.Resolve("mvno-alpha", "CreateSubscriber5G", "")
	if err != nil || a.Tenant != "mvno-alpha" {
		t.Fatalf("alpha: %+v %v", a, err)
	}
	b, err := c.Resolve("mvno-beta", "CreateSubscriber5G", "")
	if err != nil || b.Tenant != "GLOBAL" || b.Version != "3.2.0" {
		t.Fatalf("beta: %+v %v", b, err)
	}
	if _, err := c.Resolve("mvno-beta", "CreateSubscriber5G", "9.9.9"); !errors.Is(err, ErrUnknownSpec) {
		t.Fatalf("want ErrUnknownSpec, got %v", err)
	}
	if _, err := c.Resolve("host-mno", "Nope", ""); !errors.Is(err, ErrUnknownSpec) {
		t.Fatal("unknown spec resolved")
	}
	if a.ID == b.ID || a.ID == "" {
		t.Fatal("spec ids must be distinct and set")
	}
	if a.Priority().String() != "p1" {
		t.Fatalf("priority %v", a.Priority())
	}
}

// TC-CAT-003: Parameter validation - required, unknown (additionalProperties=false), type, pattern, enum, range, length.
func TestTC_CAT_003_ParamValidation(t *testing.T) {
	c := mustLoad(t)
	s, _ := c.Resolve("host-mno", "CreateSubscriber5G", "")
	cases := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"missing", map[string]any{"supi": "imsi-416770000000001", "plan": "5G"}, "msisdn: required"},
		{"unknown", map[string]any{"supi": "imsi-416770000000001", "msisdn": "962790000001", "plan": "5G", "x": "1"}, "x: not allowed"},
		{"type", map[string]any{"supi": 5.0, "msisdn": "962790000001", "plan": "5G"}, "must be a string"},
		{"pattern-injection", map[string]any{"supi": "imsi-41677'; DROP--", "msisdn": "962790000001", "plan": "5G"}, "does not match pattern"},
		{"too-long", map[string]any{"supi": "imsi-416770000000001", "msisdn": "962790000001", "plan": strings.Repeat("a", 300)}, "too long"},
	}
	for _, tc := range cases {
		err := s.Validate(tc.params)
		if err == nil || !errors.Is(err, ErrParams) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v want %q", tc.name, err, tc.want)
		}
	}
	q, _ := c.Resolve("host-mno", "QoDSession", "")
	for _, p := range []map[string]any{
		{"msisdn": "962790000001", "profile": "QOS_X", "durationSec": 3600.0},
		{"msisdn": "962790000001", "profile": "QOS_L", "durationSec": 10.0},
		{"msisdn": "962790000001", "profile": "QOS_L", "durationSec": 99999.0},
		{"msisdn": "962790000001", "profile": "QOS_L", "durationSec": 60.5},
		{"msisdn": "962790000001", "profile": "QOS_L", "durationSec": "60"},
	} {
		if err := q.Validate(p); err == nil {
			t.Errorf("expected failure for %v", p)
		}
	}
	if err := q.Validate(map[string]any{"msisdn": "962790000001", "profile": "QOS_L", "durationSec": json.Number("120")}); err != nil {
		t.Errorf("json.Number: %v", err)
	}
	f, _ := c.Resolve("host-mno", "ActivateFTTH", "")
	if err := f.Validate(map[string]any{"ontSerial": "HWTC1A2B3C4D", "oltPort": "1/1/7", "vlan": 100, "cpeEndpoint": "os::00D09E-CPE0001", "speedMbps": int64(100)}); err != nil {
		t.Errorf("int types: %v", err)
	}
	b := &ParamSchema{Type: "boolean"}
	if b.check(true) != "" || b.check("true") == "" {
		t.Error("boolean check")
	}
}

// TC-CAT-004: Template rendering escapes injected JSON (no payload injection) and rejects invalid output.
func TestTC_CAT_004_TemplateEscaping(t *testing.T) {
	c := mustLoad(t)
	out, err := c.Render("bss/bundle.json.tmpl", RenderData{Tenant: "t", Params: map[string]any{
		"plan": `x","admin":true,"y":"`, "msisdn": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, injected := m["admin"]; injected || m["name"] != `x","admin":true,"y":"` {
		t.Fatalf("injection not escaped: %s", out)
	}
	if _, err := c.Render("missing.tmpl", RenderData{}); !errors.Is(err, ErrTemplate) {
		t.Fatal("missing template")
	}
	if _, err := c.Render("bss/bundle.json.tmpl", RenderData{Params: map[string]any{}}); !errors.Is(err, ErrTemplate) {
		t.Fatal("missingkey=error expected")
	}
	if out, _ := c.Render("", RenderData{}); string(out) != "{}" {
		t.Fatal("empty template should render {}")
	}
}

// TC-CAT-007: A value taken from a completed dependency (eSIM download -> iccid) must be present: a missing or
// empty value fails the render instead of sending JSON null to the NE.
func TestTC_CAT_007_RequiredDependencyValue(t *testing.T) {
	c := mustLoad(t)
	params := map[string]any{"eid": "89049032000001000000000000000001"}
	for name, res := range map[string]map[string]map[string]any{
		"no results":   nil,
		"no iccid":     {"download": {}},
		"empty iccid":  {"download": {"iccid": ""}},
		"other branch": {"reserve": {"iccid": "8988211000000000001"}},
	} {
		for _, tmpl := range []string{"esim/confirm-order.json.tmpl", "esim/cancel-order.json.tmpl"} {
			_, err := c.Render(tmpl, RenderData{Tenant: "mvno-alpha", TaskID: "t1", Params: params, Results: res})
			if !errors.Is(err, ErrTemplate) || !strings.Contains(err.Error(), "download.iccid") {
				t.Fatalf("%s %s: %v", name, tmpl, err)
			}
		}
	}
	out, err := c.Render("esim/confirm-order.json.tmpl", RenderData{Tenant: "mvno-alpha", TaskID: "t1", Params: params,
		Results: map[string]map[string]any{"download": {"iccid": "8988211000000000001"}}})
	var m map[string]any
	if err != nil || json.Unmarshal(out, &m) != nil || m["iccid"] != "8988211000000000001" {
		t.Fatal(string(out), err)
	}
}

const goodSpec = `spec: S
version: "1.0.0"
params: {required: [a], properties: {a: {type: string}}}
entityKeys: ["'a:' + params.a"]
tasks:
  - {id: t1, ne: {nfType: X}, domain: d, op: o, template: x.tmpl}
`

func fsWith(spec, tmpl string) fstest.MapFS {
	return fstest.MapFS{
		"specs/a.yaml":     {Data: []byte(spec)},
		"templates/x.tmpl": {Data: []byte(tmpl)},
	}
}

// TC-CAT-005: Malformed specs are rejected at load (unknown fields, bad CEL, bad types, unknown templates, cycles).
func TestTC_CAT_005_InvalidSpecsRejected(t *testing.T) {
	if c, err := LoadFS(fsWith(goodSpec, `{"a": {{ json .Params.a }} }`)); err != nil || len(c.All()) != 1 || c.All()[0].Tenant != "GLOBAL" {
		t.Fatalf("good spec: %v", err)
	}
	bad := map[string]string{
		"unknown field":  goodSpec + "bogus: 1\n",
		"bad cel":        strings.Replace(goodSpec, `"'a:' + params.a"`, `"params.a +"`, 1),
		"cel not string": strings.Replace(goodSpec, `"'a:' + params.a"`, `"1 + 2"`, 1),
		"bad type":       strings.Replace(goodSpec, "type: string", "type: object", 1),
		"bad pattern":    strings.Replace(goodSpec, "type: string}", "type: string, pattern: \"[\"}", 1),
		"req undeclared": strings.Replace(goodSpec, "required: [a]", "required: [b]", 1),
		"no tasks":       strings.Split(goodSpec, "tasks:")[0],
		"bad template":   strings.Replace(goodSpec, "x.tmpl", "y.tmpl", 1),
		"forward dep":    goodSpec + "  - {id: t2, ne: {nfType: X}, domain: d, op: o, dependsOn: [t3]}\n",
		"dup task":       goodSpec + "  - {id: t1, ne: {nfType: X}, domain: d, op: o}\n",
		"bad when":       goodSpec + "  - {id: t2, ne: {nfType: X}, domain: d, op: o, when: \"1 + 2\"}\n",
		"bad coalesce":   goodSpec + "  - {id: t2, ne: {nfType: X}, domain: d, op: o, coalesceKey: \"(\"}\n",
		"bad txmode":     goodSpec + "txMode: MAYBE\n",
		"bad priority":   goodSpec + "priorityDefault: P9\n",
		"bad tenant":     goodSpec + "tenant: \"a b\"\n",
		"bad yaml":       "spec: [",
	}
	for name, s := range bad {
		if _, err := LoadFS(fsWith(s, `{}`)); err == nil {
			t.Errorf("%s: expected load error", name)
		}
	}
	if _, err := LoadFS(fsWith(goodSpec, `{{ .Params.a `)); err == nil {
		t.Error("bad template syntax accepted")
	}
	c, _ := LoadFS(fsWith(goodSpec, `{"a": {{ .Params.a }} }`))
	if _, err := c.Render("x.tmpl", RenderData{Params: map[string]any{"a": "not json"}}); !errors.Is(err, ErrTemplate) {
		t.Error("invalid JSON output accepted")
	}
}

// TC-CAT-006: CEL evaluation - when conditions, coalesce keys and entity key errors.
func TestTC_CAT_006_CEL(t *testing.T) {
	c := mustLoad(t)
	s, _ := c.Resolve("host-mno", "CreateSubscriber5G", "")
	var policy, am *TaskSpec
	for _, ts := range s.Tasks {
		switch ts.ID {
		case "policy":
			policy = ts
		case "am":
			am = ts
		}
	}
	p := map[string]any{"supi": "imsi-416770000000001", "msisdn": "962790000001", "plan": "4G-10GB"}
	if ok, _ := policy.Applies("host-mno", p); ok {
		t.Error("policy should not apply to 4G plan")
	}
	p["plan"] = "5G-1"
	if ok, _ := policy.Applies("host-mno", p); !ok {
		t.Error("policy should apply to 5G plan")
	}
	if ck, _ := am.CoalesceKeyFor("host-mno", p); ck != "udr-am:imsi-416770000000001" {
		t.Errorf("coalesce key %q", ck)
	}
	if ck, _ := policy.CoalesceKeyFor("host-mno", p); ck != "" {
		t.Error("no coalesce key expected")
	}
	if _, err := s.EntityKeysFor("host-mno", map[string]any{}); err == nil {
		t.Error("entity key with missing param must fail")
	}
	if _, err := policy.Applies("host-mno", map[string]any{}); err == nil {
		t.Error("when with missing param must fail")
	}
	if _, err := am.CoalesceKeyFor("host-mno", map[string]any{}); err == nil {
		t.Error("coalesce with missing param must fail")
	}
}

// TC-CAT-008: Spec versions compare numerically per segment (1.9 < 1.10, 1.0 < 1.0.1), so the newest active
// version is selected; parameter error and default helpers.
func TestVersionLess(t *testing.T) {
	for _, c := range [][2]string{{"1.0.0", "1.0.1"}, {"1.9", "1.10"}, {"1.0", "1.0.1"}, {"3.2.0-a", "3.2.0-b"}} {
		if !versionLess(c[0], c[1]) || versionLess(c[1], c[0]) {
			t.Errorf("%v", c)
		}
	}
	if versionLess("1.0", "1.0") {
		t.Error("equal")
	}
	if (&ParamError{Problems: []string{"a"}}).Error() == "" || defaultStr("", "d") != "d" {
		t.Error("helpers")
	}
}
