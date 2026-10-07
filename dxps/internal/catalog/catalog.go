// Package catalog loads DxPS command specs (YAML) and payload templates, resolves them per tenant
// (tenant override first, then GLOBAL), validates parameters and evaluates CEL expressions.
package catalog

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"cel.dev/cel-go/cel"
	"gopkg.in/yaml.v3"

	"dxps/internal/contract"
	"dxps/internal/ids"
	"dxps/internal/tenant"
)

//go:embed data
var embedded embed.FS

var (
	ErrUnknownSpec = errors.New("DXPS-1001: unknown command spec / version")
	ErrParams      = errors.New("DXPS-1002: parameter validation failed")
)

const maxStringLen = 256

type ParamSchema struct {
	Type      string   `yaml:"type" json:"type"`
	Pattern   string   `yaml:"pattern,omitempty" json:"pattern,omitempty"`
	Enum      []string `yaml:"enum,omitempty" json:"enum,omitempty"`
	MaxLength int      `yaml:"maxLength,omitempty" json:"maxLength,omitempty"`
	Minimum   *float64 `yaml:"minimum,omitempty" json:"minimum,omitempty"`
	Maximum   *float64 `yaml:"maximum,omitempty" json:"maximum,omitempty"`
	re        *regexp.Regexp
}

type Schema struct {
	Required   []string                `yaml:"required" json:"required"`
	Properties map[string]*ParamSchema `yaml:"properties" json:"properties"`
}

type NESelector struct {
	NFType string `yaml:"nfType" json:"nfType"`
}

type TaskSpec struct {
	ID                 string     `yaml:"id" json:"id"`
	NE                 NESelector `yaml:"ne" json:"ne"`
	Domain             string     `yaml:"domain" json:"domain"`
	Op                 string     `yaml:"op" json:"op"`
	Template           string     `yaml:"template,omitempty" json:"template,omitempty"`
	Compensate         string     `yaml:"compensate,omitempty" json:"compensate,omitempty"`
	CompensateTemplate string     `yaml:"compensateTemplate,omitempty" json:"compensateTemplate,omitempty"`
	DependsOn          []string   `yaml:"dependsOn,omitempty" json:"dependsOn,omitempty"`
	When               string     `yaml:"when,omitempty" json:"when,omitempty"`
	CoalesceKey        string     `yaml:"coalesceKey,omitempty" json:"coalesceKey,omitempty"`
	Pivot              bool       `yaml:"pivot,omitempty" json:"pivot,omitempty"`

	when, coalesce cel.Program
}

type Spec struct {
	ID              string          `yaml:"-" json:"id"`
	Tenant          string          `yaml:"tenant" json:"tenant"`
	Code            string          `yaml:"spec" json:"spec"`
	Version         string          `yaml:"version" json:"version"`
	Status          string          `yaml:"status" json:"status"`
	TxMode          contract.TxMode `yaml:"txMode" json:"txMode"`
	PriorityDefault string          `yaml:"priorityDefault" json:"priorityDefault"`
	Preempt         bool            `yaml:"preempt" json:"preempt"`
	Description     string          `yaml:"description" json:"description"`
	Params          Schema          `yaml:"params" json:"params"`
	EntityKeys      []string        `yaml:"entityKeys" json:"entityKeys"`
	Tasks           []*TaskSpec     `yaml:"tasks" json:"tasks"`
	Example         map[string]any  `yaml:"example,omitempty" json:"example,omitempty"`

	entityKeys []cel.Program
	priority   contract.Priority
}

func (s *Spec) Priority() contract.Priority { return s.priority }

type Catalog struct {
	specs     map[string][]*Spec // key tenant|code -> versions (desc)
	templates *template.Template
	env       *cel.Env
}

// Load reads the embedded catalog.
func Load() (*Catalog, error) {
	sub, err := fs.Sub(embedded, "data")
	if err != nil {
		return nil, err
	}
	return LoadFS(sub)
}

// LoadFS reads specs/*.yaml and templates/**/*.tmpl from fsys.
func LoadFS(fsys fs.FS) (*Catalog, error) {
	env, err := cel.NewEnv(
		cel.Variable("params", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("tenant", cel.StringType),
	)
	if err != nil {
		return nil, err
	}
	c := &Catalog{specs: map[string][]*Spec{}, env: env, templates: template.New("root").Funcs(funcs)}
	err = fs.WalkDir(fsys, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".tmpl") {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		_, err = c.templates.New(strings.TrimPrefix(p, "templates/")).Option("missingkey=error").Parse(string(b))
		return err
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	files, err := fs.Glob(fsys, "specs/*.yaml")
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		for {
			var s Spec
			if err := dec.Decode(&s); err != nil {
				if err.Error() == "EOF" {
					break
				}
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			if err := c.add(&s); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", f, s.Code, err)
			}
		}
	}
	for _, v := range c.specs {
		sort.Slice(v, func(i, j int) bool { return versionLess(v[j].Version, v[i].Version) })
	}
	return c, nil
}

func (c *Catalog) compile(expr string, want *cel.Type) (cel.Program, error) {
	ast, iss := c.env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	if ast.OutputType() != want && ast.OutputType() != cel.DynType {
		return nil, fmt.Errorf("expression %q must return %s", expr, want)
	}
	return c.env.Program(ast, cel.CostLimit(10000))
}

func (c *Catalog) add(s *Spec) error {
	if s.Tenant == "" {
		s.Tenant = tenant.Global
	}
	if !tenant.Valid(s.Tenant) || s.Code == "" || s.Version == "" || len(s.Tasks) == 0 || len(s.EntityKeys) == 0 {
		return errors.New("tenant, spec, version, entityKeys and tasks are required")
	}
	if s.TxMode == "" {
		s.TxMode = contract.Atomic
	}
	if !s.TxMode.Valid() {
		return fmt.Errorf("invalid txMode %q", s.TxMode)
	}
	if s.Status == "" {
		s.Status = "ACTIVE"
	}
	p, err := contract.ParsePriority(defaultStr(s.PriorityDefault, "P2"))
	if err != nil {
		return err
	}
	s.priority = p
	for name, ps := range s.Params.Properties {
		switch ps.Type {
		case "string", "integer", "number", "boolean":
		default:
			return fmt.Errorf("param %s: unsupported type %q", name, ps.Type)
		}
		if ps.Pattern != "" {
			if ps.re, err = regexp.Compile(ps.Pattern); err != nil {
				return fmt.Errorf("param %s: %w", name, err)
			}
		}
	}
	for _, r := range s.Params.Required {
		if s.Params.Properties[r] == nil {
			return fmt.Errorf("required param %s not declared", r)
		}
	}
	for _, e := range s.EntityKeys {
		prg, err := c.compile(e, cel.StringType)
		if err != nil {
			return err
		}
		s.entityKeys = append(s.entityKeys, prg)
	}
	seen := map[string]bool{}
	for _, t := range s.Tasks {
		if t.ID == "" || t.Op == "" || t.Domain == "" || t.NE.NFType == "" || seen[t.ID] {
			return fmt.Errorf("task %q: id (unique), op, domain and ne.nfType are required", t.ID)
		}
		for _, d := range t.DependsOn {
			if !seen[d] {
				return fmt.Errorf("task %s depends on unknown or later task %s", t.ID, d)
			}
		}
		seen[t.ID] = true
		for _, tp := range []string{t.Template, t.CompensateTemplate} {
			if tp != "" && c.templates.Lookup(tp) == nil {
				return fmt.Errorf("task %s: template %s not found", t.ID, tp)
			}
		}
		if t.When != "" {
			if t.when, err = c.compile(t.When, cel.BoolType); err != nil {
				return err
			}
		}
		if t.CoalesceKey != "" {
			if t.coalesce, err = c.compile(t.CoalesceKey, cel.StringType); err != nil {
				return err
			}
		}
	}
	s.ID = ids.Derive("command_spec", s.Tenant, s.Code, s.Version).String()
	k := s.Tenant + "|" + s.Code
	c.specs[k] = append(c.specs[k], s)
	return nil
}

// Resolve returns the tenant's own spec if published, otherwise the GLOBAL default. version "" = latest.
func (c *Catalog) Resolve(tenantID, code, version string) (*Spec, error) {
	for _, t := range []string{tenantID, tenant.Global} {
		for _, s := range c.specs[t+"|"+code] {
			if (version == "" || s.Version == version) && s.Status == "ACTIVE" {
				return s, nil
			}
		}
	}
	return nil, ErrUnknownSpec
}

// All returns every spec (for seeding command_spec).
func (c *Catalog) All() []*Spec {
	var out []*Spec
	for _, v := range c.specs {
		out = append(out, v...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// ---------------------------------------------------------------- params

type ParamError struct{ Problems []string }

func (e *ParamError) Error() string { return ErrParams.Error() + ": " + strings.Join(e.Problems, "; ") }
func (e *ParamError) Unwrap() error { return ErrParams }

// Validate enforces the schema with additionalProperties=false semantics.
func (s *Spec) Validate(params map[string]any) error {
	var probs []string
	for _, r := range s.Params.Required {
		if _, ok := params[r]; !ok {
			probs = append(probs, r+": required")
		}
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ps := s.Params.Properties[k]
		if ps == nil {
			probs = append(probs, k+": not allowed")
			continue
		}
		if p := ps.check(params[k]); p != "" {
			probs = append(probs, k+": "+p)
		}
	}
	if len(probs) > 0 {
		return &ParamError{Problems: probs}
	}
	return nil
}

func (ps *ParamSchema) check(v any) string {
	switch ps.Type {
	case "string":
		sv, ok := v.(string)
		if !ok {
			return "must be a string"
		}
		max := ps.MaxLength
		if max == 0 || max > maxStringLen {
			max = maxStringLen
		}
		if len(sv) > max {
			return "too long"
		}
		if ps.re != nil && !ps.re.MatchString(sv) {
			return "does not match pattern"
		}
		if len(ps.Enum) > 0 && !contains(ps.Enum, sv) {
			return "not an allowed value"
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return "must be a boolean"
		}
	case "integer", "number":
		f, ok := toFloat(v)
		if !ok {
			return "must be a number"
		}
		if ps.Type == "integer" && f != float64(int64(f)) {
			return "must be an integer"
		}
		if ps.Minimum != nil && f < *ps.Minimum {
			return "below minimum"
		}
		if ps.Maximum != nil && f > *ps.Maximum {
			return "above maximum"
		}
	}
	return ""
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// ---------------------------------------------------------------- CEL

func vars(tenantID string, params map[string]any) map[string]any {
	return map[string]any{"tenant": tenantID, "params": params}
}

func evalString(p cel.Program, v map[string]any) (string, error) {
	out, _, err := p.Eval(v)
	if err != nil {
		return "", err
	}
	s, ok := out.Value().(string)
	if !ok || s == "" {
		return "", errors.New("expression did not return a non-empty string")
	}
	return s, nil
}

// EntityKeys evaluates the spec's entity key expressions (first = primary key).
func (s *Spec) EntityKeysFor(tenantID string, params map[string]any) ([]string, error) {
	v := vars(tenantID, params)
	out := make([]string, 0, len(s.entityKeys))
	for _, p := range s.entityKeys {
		k, err := evalString(p, v)
		if err != nil {
			return nil, fmt.Errorf("%w: entity key: %v", ErrParams, err)
		}
		out = append(out, k)
	}
	return out, nil
}

// Applies evaluates a task's "when" condition.
func (t *TaskSpec) Applies(tenantID string, params map[string]any) (bool, error) {
	if t.when == nil {
		return true, nil
	}
	out, _, err := t.when.Eval(vars(tenantID, params))
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	return ok && b, nil
}

func (t *TaskSpec) CoalesceKeyFor(tenantID string, params map[string]any) (string, error) {
	if t.coalesce == nil {
		return "", nil
	}
	return evalString(t.coalesce, vars(tenantID, params))
}

// ---------------------------------------------------------------- templates

type RenderData struct {
	Tenant          string
	TaskID          string
	Params          map[string]any
	NECode          string
	SubscriberGroup string
	Results         map[string]map[string]any // spec task id -> NE response of completed dependencies
}

var funcs = template.FuncMap{
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
	"default": func(def, v any) any {
		if v == nil || v == "" {
			return def
		}
		return v
	},
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
	// need fails rendering when a value (typically a dependency result) is missing.
	"need": func(name string, v any) (any, error) {
		if v == nil || v == "" {
			return nil, fmt.Errorf("required value %s is missing", name)
		}
		return v, nil
	},
}

var ErrTemplate = errors.New("template rendering failed")

// Render executes a payload template; the result must be valid JSON (returned compacted).
func (c *Catalog) Render(name string, d RenderData) (json.RawMessage, error) {
	if name == "" {
		return json.RawMessage("{}"), nil
	}
	t := c.templates.Lookup(name)
	if t == nil {
		return nil, fmt.Errorf("%w: %s not found", ErrTemplate, name)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTemplate, err)
	}
	var out bytes.Buffer
	if err := json.Compact(&out, buf.Bytes()); err != nil {
		return nil, fmt.Errorf("%w: %s produced invalid JSON: %v", ErrTemplate, path.Base(name), err)
	}
	return out.Bytes(), nil
}

// ---------------------------------------------------------------- helpers

func defaultStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// versionLess compares dotted numeric versions (non-numeric parts compare as strings).
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		xi, e1 := strconv.Atoi(x)
		yi, e2 := strconv.Atoi(y)
		if e1 == nil && e2 == nil {
			if xi != yi {
				return xi < yi
			}
			continue
		}
		if x != y {
			return x < y
		}
	}
	return false
}
