package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type pkgReport struct {
	Name       string  `json:"name"`
	Tests      int     `json:"tests"`
	Passed     int     `json:"passed"`
	Failed     int     `json:"failed"`
	Skipped    int     `json:"skipped"`
	Coverage   float64 `json:"coverage"`
	Statements int     `json:"statements,omitempty"`
}

type testResult struct {
	Package string  `json:"package"`
	Test    string  `json:"test"`
	Action  string  `json:"action"`
	Elapsed float64 `json:"elapsed"`
}

type testTotals struct {
	Tests   int `json:"tests"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

type testReport struct {
	GeneratedAt string       `json:"generatedAt"`
	ElapsedSec  float64      `json:"elapsedSec"`
	Totals      testTotals   `json:"totals"`
	Coverage    float64      `json:"coverage"`
	Statements  int          `json:"statements"`
	Packages    []*pkgReport `json:"packages"`
	Results     []testResult `json:"results"`
}

var coverLine = regexp.MustCompile(`^(.+\.go):(\S+) (\d+) (\d+)$`)

// cmdTestReport turns `go test -json` output and a -coverpkg profile into results/tests.json (mosaic Tests tile,
// test-case document). A coverage block counts as covered if any test binary executed it.
func cmdTestReport(args []string) error {
	fs := flag.NewFlagSet("testreport", flag.ExitOnError)
	raw := fs.String("json", "", "go test -json output file")
	prof := fs.String("cover", "", "coverage profile (-coverprofile with -coverpkg)")
	out := fs.String("out", "", "tests.json to write")
	elapsed := fs.Float64("elapsed", 0, "suite wall time in seconds")
	fs.Parse(args)
	if *raw == "" || *prof == "" || *out == "" {
		return fmt.Errorf("usage: dxpsctl testreport -json raw.jsonl -cover coverage.out -out tests.json")
	}
	pk := map[string]*pkgReport{}
	get := func(n string) *pkgReport {
		if pk[n] == nil {
			pk[n] = &pkgReport{Name: n}
		}
		return pk[n]
	}
	rep := &testReport{GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), ElapsedSec: math.Round(*elapsed*10) / 10}

	f, err := os.Open(*raw) // #nosec G304 -- operator CLI flag
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e struct {
			Package, Test, Action string
			Elapsed               float64
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Test == "" {
			continue
		}
		if e.Action != "pass" && e.Action != "fail" && e.Action != "skip" {
			continue
		}
		p := get(e.Package)
		p.Tests++
		switch e.Action {
		case "pass":
			p.Passed++
		case "fail":
			p.Failed++
		default:
			p.Skipped++
		}
		rep.Results = append(rep.Results, testResult{e.Package, e.Test, e.Action, e.Elapsed})
	}
	f.Close()
	if err := sc.Err(); err != nil {
		return err
	}

	type block struct {
		n   int
		hit bool
	}
	blocks := map[string]*block{}
	files := map[string]string{}
	cf, err := os.Open(*prof) // #nosec G304 -- operator CLI flag
	if err != nil {
		return err
	}
	sc = bufio.NewScanner(cf)
	for sc.Scan() {
		m := coverLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		key := m[1] + ":" + m[2]
		n, _ := strconv.Atoi(m[3])
		cnt, _ := strconv.Atoi(m[4])
		if b := blocks[key]; b != nil {
			b.hit = b.hit || cnt > 0
		} else {
			blocks[key] = &block{n, cnt > 0}
			files[key] = m[1]
		}
	}
	cf.Close()
	stmts, covered := map[string]int{}, map[string]int{}
	for k, b := range blocks {
		p := path.Dir(files[k])
		if strings.Contains(p, "/testenv") {
			continue
		}
		stmts[p] += b.n
		rep.Statements += b.n
		if b.hit {
			covered[p] += b.n
			rep.Coverage += float64(b.n)
		}
	}
	if rep.Statements > 0 {
		rep.Coverage = math.Round(1000*rep.Coverage/float64(rep.Statements)) / 10
	}
	for p, n := range stmts {
		r := get(p)
		r.Statements = n
		r.Coverage = math.Round(1000*float64(covered[p])/float64(n)) / 10
	}
	for _, p := range pk {
		if strings.HasPrefix(p.Name, "dxps/internal/") && !strings.Contains(p.Name, "/testenv") {
			rep.Packages = append(rep.Packages, p)
			rep.Totals.Tests += p.Tests
			rep.Totals.Passed += p.Passed
			rep.Totals.Failed += p.Failed
			rep.Totals.Skipped += p.Skipped
		}
	}
	sort.Slice(rep.Packages, func(i, j int) bool { return rep.Packages[i].Name < rep.Packages[j].Name })
	b, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, b, 0o600); err != nil {
		return err
	}
	for _, p := range rep.Packages {
		fmt.Printf("%-34s %4d tests %4d pass %3d fail %3d skip %6.1f%%\n", strings.TrimPrefix(p.Name, "dxps/internal/"),
			p.Tests, p.Passed, p.Failed, p.Skipped, p.Coverage)
	}
	fmt.Printf("TOTAL: %d tests, %d passed, %d failed, %d skipped, coverage %.1f%% of %d statements, %.1fs\n",
		rep.Totals.Tests, rep.Totals.Passed, rep.Totals.Failed, rep.Totals.Skipped, rep.Coverage, rep.Statements, rep.ElapsedSec)
	if rep.Totals.Failed > 0 {
		fmt.Println("FAILED:")
		for _, r := range rep.Results {
			if r.Action == "fail" {
				fmt.Println("  " + r.Package + " " + r.Test)
			}
		}
		return fmt.Errorf("%d test(s) failed", rep.Totals.Failed)
	}
	return nil
}
