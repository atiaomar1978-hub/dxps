package console

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing/fstest"
	"time"

	"gopkg.in/yaml.v3"

	"dxps/internal/catalog"
	"dxps/internal/registry"
)

// Source is the catalog source tree (specs/*.yaml, templates/**/*.tmpl), normally dxps/internal/catalog/data,
// which the services embed at build time.
type Source struct {
	FS fs.FS
}

// Files returns every catalog file of the source tree.
func (s *Source) Files() (map[string]string, error) {
	out := map[string]string{}
	err := fs.WalkDir(s.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasPrefix(p, "specs/") && !strings.HasPrefix(p, "templates/") {
			return nil
		}
		b, err := fs.ReadFile(s.FS, p)
		if err != nil {
			return err
		}
		out[p] = string(b)
		return nil
	})
	return out, err
}

func (s *Source) Load() (*catalog.Catalog, error) { return catalog.LoadFS(s.FS) }

// SpecSummary is one catalog spec as listed by the console.
type SpecSummary struct {
	Tenant      string   `json:"tenant"`
	Spec        string   `json:"spec"`
	Version     string   `json:"version"`
	Status      string   `json:"status"`
	TxMode      string   `json:"txMode"`
	Priority    string   `json:"priority"`
	Description string   `json:"description"`
	Tasks       int      `json:"tasks"`
	Domains     []string `json:"domains"`
	File        string   `json:"file"`
}

type specHead struct {
	Tenant  string `yaml:"tenant"`
	Spec    string `yaml:"spec"`
	Version string `yaml:"version"`
}

// specFiles maps tenant|spec|version to the spec file defining it.
func specFiles(files map[string]string) map[string]string {
	out := map[string]string{}
	for p, body := range files {
		if !strings.HasPrefix(p, "specs/") || !strings.HasSuffix(p, ".yaml") {
			continue
		}
		dec := yaml.NewDecoder(strings.NewReader(body))
		for {
			var h specHead
			if err := dec.Decode(&h); err != nil {
				break
			}
			if h.Tenant == "" {
				h.Tenant = "GLOBAL"
			}
			out[h.Tenant+"|"+h.Spec+"|"+h.Version] = p
		}
	}
	return out
}

func summarize(cat *catalog.Catalog, files map[string]string) []SpecSummary {
	where := specFiles(files)
	var out []SpecSummary
	for _, s := range cat.All() {
		doms := map[string]bool{}
		for _, t := range s.Tasks {
			doms[t.Domain] = true
		}
		var dl []string
		for d := range doms {
			dl = append(dl, d)
		}
		sort.Strings(dl)
		out = append(out, SpecSummary{Tenant: s.Tenant, Spec: s.Code, Version: s.Version, Status: s.Status, TxMode: string(s.TxMode),
			Priority: s.Priority().String(), Description: s.Description, Tasks: len(s.Tasks), Domains: dl,
			File: where[s.Tenant+"|"+s.Code+"|"+s.Version]})
	}
	return out
}

func findSpec(cat *catalog.Catalog, tenantID, code, version string) *catalog.Spec {
	for _, s := range cat.All() {
		if s.Tenant == tenantID && s.Code == code && s.Version == version {
			return s
		}
	}
	return nil
}

// ---------------------------------------------------------------- drafts

var (
	reDraftPath = regexp.MustCompile(`^(specs/[A-Za-z0-9_.-]+\.yaml|templates/[a-z0-9_-]+/[A-Za-z0-9_.-]+\.json\.tmpl)$`)
	reDraftID   = regexp.MustCompile(`^[a-f0-9]{16}$`)
)

const (
	maxDraftFile  = 256 << 10
	maxDraftFiles = 64
)

// Draft is a working copy of catalog files: only changed or added files are stored; deleted is the list of
// source files removed by the draft.
type Draft struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Author  string            `json:"author"`
	Created time.Time         `json:"created"`
	Updated time.Time         `json:"updated"`
	Files   map[string]string `json:"files"`
	Deleted []string          `json:"deleted,omitempty"`
}

type DraftSummary struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Author  string    `json:"author"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	Files   []string  `json:"files"`
	Deleted []string  `json:"deleted,omitempty"`
}

func (d *Draft) Summary() DraftSummary {
	fl := make([]string, 0, len(d.Files))
	for p := range d.Files {
		fl = append(fl, p)
	}
	sort.Strings(fl)
	return DraftSummary{ID: d.ID, Name: d.Name, Author: d.Author, Created: d.Created, Updated: d.Updated, Files: fl, Deleted: d.Deleted}
}

// CheckFiles validates draft file names and sizes.
func CheckFiles(files map[string]string, deleted []string) error {
	if len(files)+len(deleted) > maxDraftFiles {
		return fmt.Errorf("a draft can change at most %d files", maxDraftFiles)
	}
	for p, body := range files {
		if !reDraftPath.MatchString(p) || strings.Contains(p, "..") {
			return fmt.Errorf("invalid file name %q (specs/<name>.yaml or templates/<domain>/<name>.json.tmpl)", p)
		}
		if len(body) > maxDraftFile {
			return fmt.Errorf("%s is larger than %d KiB", p, maxDraftFile>>10)
		}
	}
	for _, p := range deleted {
		if !reDraftPath.MatchString(p) {
			return fmt.Errorf("invalid file name %q", p)
		}
	}
	return nil
}

// Drafts stores drafts as JSON files (mode 0600) in dir.
type Drafts struct {
	dir string
	now func() time.Time
	mu  sync.Mutex
}

func NewDrafts(dir string) *Drafts { return &Drafts{dir: dir, now: time.Now} }

var ErrNoDraft = errors.New("draft not found")

func (d *Drafts) file(id string) (string, error) {
	if !reDraftID.MatchString(id) {
		return "", ErrNoDraft
	}
	return filepath.Join(d.dir, id+".json"), nil
}

func (d *Drafts) write(x *Draft) error {
	if err := os.MkdirAll(d.dir, 0o700); err != nil {
		return err
	}
	f, err := d.file(x.ID)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(x, "", "  ")
	if err != nil {
		return err
	}
	tmp := f + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f)
}

func (d *Drafts) Create(name, author string, files map[string]string) (*Draft, error) {
	if strings.TrimSpace(name) == "" || len(name) > 80 {
		return nil, errors.New("draft name: 1-80 characters")
	}
	if err := CheckFiles(files, nil); err != nil {
		return nil, err
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	now := d.now().UTC()
	x := &Draft{ID: hex.EncodeToString(b), Name: strings.TrimSpace(name), Author: author, Created: now, Updated: now, Files: files}
	if x.Files == nil {
		x.Files = map[string]string{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return x, d.write(x)
}

func (d *Drafts) Get(id string) (*Draft, error) {
	f, err := d.file(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(f) // #nosec G304 -- id is validated hex
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoDraft
	}
	if err != nil {
		return nil, err
	}
	var x Draft
	if err := json.Unmarshal(b, &x); err != nil {
		return nil, err
	}
	if x.Files == nil {
		x.Files = map[string]string{}
	}
	return &x, nil
}

func (d *Drafts) Save(id string, files map[string]string, deleted []string) (*Draft, error) {
	if err := CheckFiles(files, deleted); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	x, err := d.Get(id)
	if err != nil {
		return nil, err
	}
	x.Files, x.Deleted, x.Updated = files, deleted, d.now().UTC()
	if x.Files == nil {
		x.Files = map[string]string{}
	}
	return x, d.write(x)
}

func (d *Drafts) Delete(id string) error {
	f, err := d.file(id)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := os.Remove(f); errors.Is(err, os.ErrNotExist) {
		return ErrNoDraft
	} else if err != nil {
		return err
	}
	return nil
}

func (d *Drafts) List() ([]DraftSummary, error) {
	ents, err := os.ReadDir(d.dir)
	if errors.Is(err, os.ErrNotExist) {
		return []DraftSummary{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []DraftSummary{}
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		if x, err := d.Get(id); err == nil {
			out = append(out, x.Summary())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// Overlay returns the source files with the draft applied.
func Overlay(base map[string]string, d *Draft) map[string]string {
	out := make(map[string]string, len(base)+len(d.Files))
	for p, b := range base {
		out[p] = b
	}
	for _, p := range d.Deleted {
		delete(out, p)
	}
	for p, b := range d.Files {
		out[p] = b
	}
	return out
}

func mapFS(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for p, b := range files {
		m[p] = &fstest.MapFile{Data: []byte(b), Mode: 0o644}
	}
	return m
}

// ---------------------------------------------------------------- validation

type SmokeResult struct {
	Tenant   string   `json:"tenant"`
	Spec     string   `json:"spec"`
	Version  string   `json:"version"`
	OK       bool     `json:"ok"`
	Problems []string `json:"problems,omitempty"`
}

type Validation struct {
	OK      bool          `json:"ok"`
	Error   string        `json:"error,omitempty"`
	Specs   []SpecSummary `json:"specs"`
	Smoke   []SmokeResult `json:"smoke"`
	Changed []string      `json:"changed"`
}

// Validate loads the overlay with the DxPS catalog loader (YAML schema, CEL, template parsing, DAG order)
// and runs every spec's example through the Lab (parameters, NE selection, rendering of independent tasks).
func Validate(base map[string]string, d *Draft, reg *registry.Registry, labTenant string) *Validation {
	files := Overlay(base, d)
	v := &Validation{Changed: d.Summary().Files, Specs: []SpecSummary{}, Smoke: []SmokeResult{}}
	v.Changed = append(v.Changed, d.Deleted...)
	cat, err := catalog.LoadFS(mapFS(files))
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Specs = summarize(cat, files)
	v.OK = true
	for _, s := range cat.All() {
		if s.Example == nil {
			continue
		}
		t := labTenant
		if s.Tenant != "GLOBAL" {
			t = s.Tenant
		}
		res := RunLab(cat, reg, LabRequest{Tenant: t, Spec: s.Code, Version: s.Version, Params: s.Example})
		sr := SmokeResult{Tenant: t, Spec: s.Code, Version: s.Version, OK: true}
		sr.Problems = append(sr.Problems, res.Problems...)
		for _, lt := range res.Tasks {
			if lt.NEError != "" {
				sr.Problems = append(sr.Problems, lt.ID+": "+lt.NEError)
			}
			if lt.RenderError != "" && len(lt.DependsOn) == 0 {
				sr.Problems = append(sr.Problems, lt.ID+": "+lt.RenderError)
			}
		}
		sr.OK = len(sr.Problems) == 0
		v.OK = v.OK && sr.OK
		v.Smoke = append(v.Smoke, sr)
	}
	return v
}

// Changed returns the overlay files that differ from the source (for export).
func Changed(base map[string]string, d *Draft) map[string]string {
	out := map[string]string{}
	for p, b := range d.Files {
		if base[p] != b {
			out[p] = b
		}
	}
	return out
}

// SpecYAML renders a spec back to YAML (used to start a draft for a tenant override).
func SpecYAML(s *catalog.Spec) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(s); err != nil {
		return "", err
	}
	return buf.String(), enc.Close()
}

// OverrideFile is the file name used for a tenant override of a spec.
func OverrideFile(tenantID, code string) string {
	return path.Join("specs", strings.ToLower(tenantID+"-"+code)+".yaml")
}
