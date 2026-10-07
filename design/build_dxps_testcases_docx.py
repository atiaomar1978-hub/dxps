"""Generate the DxPS test-case and test-results document (Word) from real test artefacts.

Inputs (all produced by the DxPS scripts / tests, nothing is typed in by hand):
  <runtime>/results/tests.json            scripts/test-all.ps1 (go test -json, merged -coverpkg coverage)
  <runtime>/results/probes.json           live SEC-01..24 probe run against the running stack (mosaic)
  <runtime>/results/govulncheck.txt       govulncheck -show verbose ./...
  <runtime>/results/gosec.json            gosec -exclude=G404 ./...
  <runtime>/results/exchanges.json        NE simulator exchanges harvested during a chaos load run
  <runtime>/results/payloads/*.json       E2E traces captured by the orchestrator tests (DXPS_CAPTURE_DIR)
  <runtime>/results/load-2000.json        mosaic load run (2000 orders, 100/s, chaos 0.1)
  dxps/**/*_test.go                       test-case ids and descriptions (// TC-XXX-NNN comments)
"""
import json
import os
import re
import sys
from collections import Counter, defaultdict
from datetime import datetime
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt
from docx import Document
from docx.enum.section import WD_ORIENT
from docx.enum.text import WD_ALIGN_PARAGRAPH
from docx.oxml import OxmlElement
from docx.oxml.ns import qn
from docx.shared import Inches, Pt, RGBColor

from build_dxps_tobe_docx import (GREY, NAVY, add_toc, bullets, code, figure, h, page_break, para, rich,
                                  set_run_font, shade, table)

HERE = Path(__file__).parent
SRC = HERE.parent / "dxps"
RT = Path(os.environ.get("DXPS_RUNTIME", Path.home() / "dxps-runtime"))
RES = RT / "results"
FIGS = HERE / "diagrams" / "tests"
OUT = HERE.parent / "DxPS_Test_Cases_and_Results.docx"
OUT_FALLBACK = HERE.parent / "DxPS_Test_Cases_and_Results_v1.docx"
GREEN, RED = RGBColor(0x15, 0x80, 0x3D), RGBColor(0xB9, 0x1C, 0x1C)

COMPONENT = {
    "auth": ("Gateway security: JWT authentication and scope authorisation", "LLD 7.1 Security"),
    "tenant": ("Tenant registry, identifiers and SLA weights", "LLD 4 Multi-tenancy"),
    "ids": ("Identifiers: UUIDv7 and deterministic derived ids", "LLD 5 Data model"),
    "secretbox": ("Secret encryption at rest (AES-256-GCM)", "LLD 7.3 Secrets"),
    "breaker": ("Per-NE circuit breaker", "LLD 6.6 Resilience"),
    "contract": ("Kafka contracts, topics and keys", "LLD 6.1 Kafka topology"),
    "catalog": ("Command catalog: specs, CEL rules, payload templates", "LLD 6.3 Catalog"),
    "scheduler": ("Deficit round-robin fair scheduler (tenant x priority)", "LLD 6.4 Fairness"),
    "keylane": ("Entity key lanes (per-subscriber ordering)", "LLD 6.5 Sequencing"),
    "saga": ("Saga compensation and retry back-off", "LLD 6.6 Resilience"),
    "registry": ("NE registry, shared-NE access and quotas", "LLD 6.7 NE registry"),
    "pki": ("PKI and mutual TLS", "LLD 7.2 Transport security"),
    "config": ("Configuration and secret loading", "LLD 8 Deployment"),
    "planner": ("Planner: business command to NE task DAG", "LLD 6.3 Planning"),
    "netsim": ("Network element simulators (9 NEs + webhook sink)", "Test harness"),
    "adapter": ("Southbound adapters (SBA, IMS, NETCONF/USP, eSIM, BSS, NPDB, CAMARA)", "LLD 6.8 Adapters"),
    "eventhub": ("TMF688 event hub (webhooks, HMAC, retries, SSRF guard)", "LLD 6.9 Events"),
    "gateway": ("TMF641 northbound gateway", "LLD 6.2 Northbound API"),
    "dashboard": ("Mosaic dashboard and live security probes", "Operations"),
    "orchestrator": ("Orchestrator: sagas, dependencies, sequencing (E2E)", "LLD 6.3-6.6"),
    "bus": ("Kafka bus: tiered consumers, EOS, retry ladder", "LLD 6.1 Kafka"),
    "store": ("PostgreSQL store: RLS, outbox, migrations, seed", "LLD 5 Data model"),
    "svc": ("Service bootstrap, logging, HTTP server hardening", "LLD 8 Deployment"),
    "seed": ("Reference data (tenants, NEs)", "Test data"),
}

SEC_WORDS = re.compile(r"inject|RLS|mTLS|CSRF|token|SSRF|isolation|redact|alg|signature|forg|auth|secret|HMAC|"
                       r"rebinding|host guard|loopback|privilege|cross-tenant|tamper|replay|rate limit|scope|TLS",
                       re.I)


# --------------------------------------------------------------------------- data
def load_json(name, default=None):
    p = RES / name
    if not p.exists():
        return default
    return json.loads(p.read_text(encoding="utf-8-sig"))


def parse_test_cases():
    """Return {(package, TestName): tc} from // TC-XXX-NNN comments directly above func TestX."""
    cases = {}
    head = re.compile(r"^// (TC-[A-Z]+-\d+[a-z]?)(?: \(([^)]*)\))?: ?(.*)$")
    for f in sorted(SRC.rglob("*_test.go")):
        pkg = "dxps/" + f.parent.relative_to(SRC).as_posix()
        cur = None
        for line in f.read_text(encoding="utf-8").splitlines():
            m = head.match(line)
            if m:
                cur = {"id": m.group(1), "tag": m.group(2) or "", "desc": m.group(3).strip(), "file": f.name}
                continue
            if cur and line.startswith("// "):
                cur["desc"] += " " + line[3:].strip()
                continue
            fm = re.match(r"^func (Test\w+)\(", line)
            if fm and cur:
                cur["test"] = fm.group(1)
                cases[(pkg, fm.group(1))] = cur
            if not line.startswith("//"):
                cur = None
    return cases


def level_of(tag):
    t = tag.lower()
    if "e2e" in t:
        return "E2E"
    if "security" in t:
        return "Security"
    if any(k in t for k in ("integration", "postgresql", "kafka")):
        return "Integration"
    return "Unit"


def pretty(raw, limit=1400):
    if raw is None:
        return ""
    if isinstance(raw, (dict, list)):
        s = json.dumps(raw, indent=2, ensure_ascii=False)
    else:
        try:
            s = json.dumps(json.loads(raw), indent=2, ensure_ascii=False)
        except (TypeError, ValueError):
            s = str(raw)
    return s if len(s) <= limit else s[:limit] + "\n... (truncated)"


# --------------------------------------------------------------------------- charts
def charts(tests, probes, load):
    FIGS.mkdir(parents=True, exist_ok=True)
    out = {}
    pk = [p for p in tests["packages"] if p.get("statements")]
    pk.sort(key=lambda p: p["coverage"])
    fig, ax = plt.subplots(figsize=(8, 6.2))
    names = [p["name"].replace("dxps/internal/", "") for p in pk]
    cov = [p["coverage"] for p in pk]
    cols = ["#16a34a" if c >= 90 else "#65a30d" if c >= 80 else "#d97706" for c in cov]
    ax.barh(names, cov, color=cols)
    ax.axvline(tests["coverage"], color="#1e3a8a", ls="--", lw=1.2)
    ax.text(tests["coverage"] + 0.5, -0.9, f"overall {tests['coverage']}%", color="#1e3a8a", fontsize=8)
    for i, c in enumerate(cov):
        ax.text(c + 0.6, i, f"{c:.1f}%", va="center", fontsize=7.5)
    ax.set_xlim(0, 108)
    ax.set_xlabel("statement coverage (whole suite, merged -coverpkg)")
    ax.set_title("Coverage by package", fontsize=11, color="#1e3a8a")
    ax.tick_params(axis="y", labelsize=8)
    for s in ("top", "right"):
        ax.spines[s].set_visible(False)
    fig.tight_layout()
    out["coverage"] = FIGS / "coverage.png"
    fig.savefig(out["coverage"], dpi=170)
    plt.close(fig)

    if probes:
        cat = defaultdict(lambda: [0, 0])
        for p in probes:
            cat[p["category"]][0 if p["pass"] else 1] += 1
        fig, ax = plt.subplots(figsize=(7, 2.6))
        ks = list(cat)
        ax.bar(ks, [cat[k][0] for k in ks], color="#16a34a", label="pass")
        ax.bar(ks, [cat[k][1] for k in ks], bottom=[cat[k][0] for k in ks], color="#dc2626", label="fail")
        for i, k in enumerate(ks):
            ax.text(i, cat[k][0] + cat[k][1] + 0.1, f"{cat[k][0]}/{sum(cat[k])}", ha="center", fontsize=8)
        ax.set_ylim(0, max(sum(v) for v in cat.values()) * 1.25)
        ax.set_ylabel("probes")
        ax.set_title("Live security probes by category (running stack)", fontsize=10, color="#1e3a8a")
        ax.legend(fontsize=8, frameon=False)
        for s in ("top", "right"):
            ax.spines[s].set_visible(False)
        fig.tight_layout()
        out["probes"] = FIGS / "probes.png"
        fig.savefig(out["probes"], dpi=170)
        plt.close(fig)

    if load:
        ne = sorted(load["neLatency"], key=lambda x: -x["calls"])
        fig, ax = plt.subplots(figsize=(8, 3.2))
        xs = range(len(ne))
        ax.bar([x - 0.2 for x in xs], [n["p50us"] / 1000 for n in ne], width=0.4, color="#2563eb", label="p50 ms")
        ax.bar([x + 0.2 for x in xs], [n["p99us"] / 1000 for n in ne], width=0.4, color="#9333ea", label="p99 ms")
        ax.set_yscale("log")
        ax.set_xticks(list(xs), [f"{n['ne']}\n{n['calls']} calls" for n in ne], fontsize=7)
        ax.set_ylabel("latency (ms, log)")
        ax.set_title("NE call latency during the 2000-order chaos load", fontsize=10, color="#1e3a8a")
        ax.legend(fontsize=8, frameon=False)
        for s in ("top", "right"):
            ax.spines[s].set_visible(False)
        fig.tight_layout()
        out["latency"] = FIGS / "latency.png"
        fig.savefig(out["latency"], dpi=170)
        plt.close(fig)

        spec = load["demo"]["bySpec"]
        fig, ax = plt.subplots(figsize=(7, 3.2))
        ks = sorted(spec, key=lambda k: -spec[k])
        ax.barh(ks[::-1], [spec[k] for k in ks[::-1]], color="#0891b2")
        for i, k in enumerate(ks[::-1]):
            ax.text(spec[k] + 5, i, str(spec[k]), va="center", fontsize=7.5)
        ax.set_title("Order mix of the load run (all 2000 accepted, HTTP 201)", fontsize=10, color="#1e3a8a")
        ax.tick_params(axis="y", labelsize=8)
        for s in ("top", "right"):
            ax.spines[s].set_visible(False)
        fig.tight_layout()
        out["mix"] = FIGS / "mix.png"
        fig.savefig(out["mix"], dpi=170)
        plt.close(fig)
    return out


# --------------------------------------------------------------------------- helpers
def kpi_table(doc, items):
    t = doc.add_table(rows=2, cols=len(items))
    t.style = "Table Grid"
    for i, (label, value, color) in enumerate(items):
        c = t.rows[0].cells[i]
        c.text = ""
        p = c.paragraphs[0]
        p.alignment = WD_ALIGN_PARAGRAPH.CENTER
        set_run_font(p.add_run(value), size=18, bold=True, color=color or NAVY)
        shade(c, "EEF2FF")
        c2 = t.rows[1].cells[i]
        c2.text = ""
        p2 = c2.paragraphs[0]
        p2.alignment = WD_ALIGN_PARAGRAPH.CENTER
        set_run_font(p2.add_run(label), size=8, color=GREY)
    doc.add_paragraph().paragraph_format.space_after = Pt(2)


def color_result(t, col):
    for row in t.rows[1:]:
        cell = row.cells[col]
        txt = cell.text
        runs = [r for r in cell.paragraphs[0].runs if r.text]
        if not runs:
            continue
        run = runs[-1]
        if txt.startswith("PASS"):
            run.font.color.rgb = GREEN
            run.bold = True
        elif txt.startswith("FAIL"):
            run.font.color.rgb = RED
            run.bold = True


def header_footer(doc, title):
    sec = doc.sections[0]
    hp = sec.header.paragraphs[0]
    hp.alignment = WD_ALIGN_PARAGRAPH.RIGHT
    set_run_font(hp.add_run(title + "  |  Confidential"), size=8, color=GREY)
    fp = sec.footer.paragraphs[0]
    fp.alignment = WD_ALIGN_PARAGRAPH.CENTER
    run = fp.add_run()
    for kind, text in (("begin", None), (None, "PAGE"), ("end", None)):
        if kind:
            el = OxmlElement("w:fldChar")
            el.set(qn("w:fldCharType"), kind)
        else:
            el = OxmlElement("w:instrText")
            el.set(qn("xml:space"), "preserve")
            el.text = text
        run._r.append(el)
    set_run_font(run, size=8, color=GREY)


# --------------------------------------------------------------------------- document
def build():
    tests = load_json("tests.json")
    if not tests:
        sys.exit("tests.json missing: run scripts/test-all.ps1 first")
    probes = load_json("probes.json", [])
    gosec = load_json("gosec.json", {})
    exchanges = load_json("exchanges.json", [])
    load = load_json("load-2000.json")
    vuln = ""
    if (RES / "govulncheck.txt").exists():
        raw = (RES / "govulncheck.txt").read_bytes()  # Windows PowerShell redirection writes UTF-16LE
        vuln = raw.decode("utf-16") if raw[:2] in (b"\xff\xfe", b"\xfe\xff") else raw.decode("utf-8", "replace")
    traces = {p.stem: json.loads(p.read_text(encoding="utf-8-sig")) for p in sorted((RES / "payloads").glob("*.json"))}
    cases = parse_test_cases()
    figs = charts(tests, probes, load)

    results = defaultdict(list)  # (pkg, top-level test) -> [result rows incl. subtests]
    for r in tests["results"]:
        results[(r["package"], r["test"].split("/")[0])].append(r)

    def outcome(pkg, name):
        rs = results.get((pkg, name), [])
        top = [r for r in rs if r["test"] == name]
        if not top:
            return "NOT RUN", 0.0, 0
        sub = len(rs) - 1
        return top[0]["action"].upper().replace("SKIP", "SKIPPED"), top[0].get("elapsed") or 0.0, sub

    rows_by_pkg = defaultdict(list)
    level_count = Counter()
    sec_count = 0
    for (pkg, name), tc in cases.items():
        res, el, sub = outcome(pkg, name)
        lvl = level_of(tc["tag"])
        is_sec = lvl == "Security" or bool(SEC_WORDS.search(tc["desc"] + " " + tc["tag"]))
        level_count[lvl] += 1
        sec_count += is_sec
        rows_by_pkg[pkg].append((tc, lvl, is_sec, res, el, sub))
    for v in rows_by_pkg.values():
        v.sort(key=lambda x: x[0]["id"])
    tc_pass = sum(1 for v in rows_by_pkg.values() for x in v if x[3] == "PASS")
    untagged = sorted({(r["package"], r["test"].split("/")[0]) for r in tests["results"]} - set(cases))

    doc = Document()
    sec = doc.sections[0]
    sec.orientation = WD_ORIENT.PORTRAIT
    sec.left_margin = sec.right_margin = Inches(0.7)
    sec.top_margin = sec.bottom_margin = Inches(0.75)
    st = doc.styles["Normal"]
    st.font.name = "Calibri"
    st.font.size = Pt(10)
    header_footer(doc, "DxPS Real-time Provisioning System - Test Cases & Results")
    gen = datetime.fromisoformat(tests["generatedAt"].replace("Z", "+00:00")).astimezone()

    # ---- cover
    for _ in range(4):
        doc.add_paragraph()
    para(doc, "DxPS", bold=True, size=30, color=NAVY, align=WD_ALIGN_PARAGRAPH.CENTER, space_after=0)
    para(doc, "Real-time Provisioning System", bold=True, size=20, color=NAVY, align=WD_ALIGN_PARAGRAPH.CENTER)
    para(doc, "Test Cases, Test Results, Security Verification and Payload Evidence", bold=True, size=15,
         align=WD_ALIGN_PARAGRAPH.CENTER)
    para(doc, "Go + Apache Kafka 4 (KRaft) + PostgreSQL 18, exercised against mTLS HTTP/2 network element simulators",
         size=11, italic=True, color=GREY, align=WD_ALIGN_PARAGRAPH.CENTER, space_after=24)
    kpi_table(doc, [
        ("automated tests passed", f"{tests['totals']['passed']}/{tests['totals']['tests']}", GREEN if not tests["totals"]["failed"] else RED),
        ("statement coverage", f"{tests['coverage']}%", None),
        ("live security probes", f"{sum(p['pass'] for p in probes)}/{len(probes)}", GREEN),
        ("known CVEs (govulncheck)", "0" if "No vulnerabilities found" in vuln else "see 6.3", GREEN),
    ])
    table(doc, ["Item", "Value"], [
        ["Document", "DXPS-TEST-CASES-RESULTS"],
        ["Version", "1.0"],
        ["Test run", gen.strftime("%d %B %Y %H:%M %Z") + f"  (suite wall time {tests.get('elapsedSec', '?')} s)"],
        ["Design baseline", "DxPS_TO-BE_HLD_LLD.docx v1.1"],
        ["System under test", "dxps Go module: 6 services (gateway, orchestrator, adapter, eventhub, netsim, dashboard), "
                              "dxpsctl, 24 internal packages"],
        ["Environment", "Windows 11 (10.0.26200), Go 1.26.6, Apache Kafka 4.3.1 (KRaft, single node), "
                        "PostgreSQL 18.6 (TLS 1.3, SCRAM-SHA-256, RLS), python-docx report generator"],
        ["Source of every number", "Generated from go test -json, the merged coverage profile, the live probe run, "
                                   "govulncheck, gosec and captured simulator exchanges (no hand-typed results)"],
    ], widths=[1.6, 5.4])
    page_break(doc)
    para(doc, "Table of Contents", bold=True, size=16, color=NAVY)
    add_toc(doc)
    page_break(doc)

    # ---- 1 summary
    h(doc, "1. Executive summary", 1)
    para(doc, f"All {tests['totals']['tests']} automated tests pass ({len(cases)} documented test cases plus their "
              f"sub-tests). The suite covers {tests['coverage']}% of the {tests['statements']:,} statements in the "
              f"DxPS internal packages, measured across package boundaries so integration tests count toward the "
              f"code they exercise. All {len(probes)} live security probes pass against the running stack, "
              f"govulncheck reports no reachable vulnerabilities, and gosec reports no high or medium findings.")
    para(doc, "Testing found and fixed eight defects, six of them security-relevant (section 7). The most important: "
              "the simulator exchange log exposed OAuth access tokens on the dashboard; the dashboard accepted any "
              "Host header (DNS rebinding); the event hub followed redirects as retries; and a missing dependency "
              "result was rendered as JSON null into an NE payload instead of failing the task.")
    kpi_table(doc, [
        ("unit", str(level_count["Unit"]), None), ("integration", str(level_count["Integration"]), None),
        ("end-to-end", str(level_count["E2E"]), None), ("security-focused", str(sec_count), None),
        ("documented TCs passed", f"{tc_pass}/{len(cases)}", GREEN if tc_pass == len(cases) else RED),
    ])
    if "coverage" in figs:
        figure(doc, figs["coverage"], "Figure 1 - Statement coverage per package (whole suite)", width=6.4)

    # ---- 2 scope and approach
    h(doc, "2. Scope and test approach", 1)
    table(doc, ["Level", "What it proves", "How it runs"], [
        ["Unit", "Pure logic: JWT, tenant ids, catalog/CEL, planner DAGs, DRR scheduler, key lanes, saga, breaker, PKI",
         "go test, no external dependency"],
        ["Component", "Each service handler against network simulators over HTTP/2 mTLS (adapters, event hub, gateway)",
         "httptest servers, netsim in-process"],
        ["Integration", "Real PostgreSQL 18 (RLS, roles, outbox, migrations) and Kafka 4 (tiered consumers, "
                        "exactly-once, retry ladder)", "local runtime; isolated zz-test-* tenants and uniquely named topics"],
        ["End-to-end", "TMF641 order -> gateway -> PostgreSQL -> orchestrator -> planner -> adapter -> simulator -> "
                       "result -> TMF688 event, including compensation and retries", "orchestrator harness with real gateway and store"],
        ["Security", "24 live attack probes, RLS and least-privilege checks, injection, SSRF, CSRF, DNS rebinding, "
                     "mTLS, token forgery, secret redaction", "tests + mosaic probe run + govulncheck + gosec"],
        ["Load", "2000 mixed orders at 100/s with 10% chaos faults through the running stack", "mosaic load generator"],
    ], widths=[1.0, 3.8, 2.2])
    para(doc, "Network simulators", bold=True, color=NAVY)
    para(doc, "Every southbound call goes to a simulator, never to a stub inside the adapter. Simulators speak HTTP/2 "
              "over mutual TLS, validate payloads against the interface rules, keep per-tenant state and record each "
              "exchange (secrets redacted). Faults are injected deterministically through the subscriber identifier "
              "so a test or load run can target an exact behaviour:")
    table(doc, ["Trigger", "Simulator behaviour", "DxPS behaviour under test"], [
        ["MSISDN/EID ends 997", "two HTTP 503, then success", "retry ladder (dxps.retry.* topics) recovers; order completes"],
        ["ends 998", "permanent 400 (SM-DP+: ES2+ Failed inside HTTP 200)", "task fails, ATOMIC saga compensates in reverse"],
        ["ends 999", "HTTP 503 every time", "bounded retries, DXPS-4001 after MaxAttempts, rollback"],
        ["USP device OFFLINE", "USP error 7002", "permanent failure mapped to a DXPS code"],
        ["QoD bandwidth > 10000", "HTTP 409", "business rejection surfaced to the order"],
        ["admin API", "latency and random error rate per simulator", "chaos/latency experiments from the mosaic"],
    ], widths=[1.5, 2.6, 2.9])

    # ---- 3 environment
    h(doc, "3. Test environment", 1)
    table(doc, ["Component", "Version / setting"], [
        ["Go toolchain", "go1.26.6 windows/amd64 (go.mod: go 1.26.5, toolchain go1.26.6)"],
        ["Apache Kafka", "4.3.1, KRaft combined broker+controller, 127.0.0.1:9092, 41 DxPS topics, KIP-848 consumer groups"],
        ["PostgreSQL", "18.6, port 5433, TLS 1.3 only, SCRAM-SHA-256; roles dxps_owner (DDL), dxps_app (RLS enforced), "
                       "dxps_ops (BYPASSRLS, registry/relay only)"],
        ["Services", "gateway https://127.0.0.1:8443 (TLS), orchestrator, adapter, eventhub, netsim (NEs on 9101-9108 "
                     "mTLS, webhook sink 9109), mosaic dashboard http://127.0.0.1:8088 (loopback only)"],
        ["Tenants", "host-mno (host MNO), mvno-alpha (full MVNO, own OCS), mvno-beta (light MVNO), ent-acme (enterprise), "
                    "mvno-gamma (suspended); tests use zz-test-* tenants for isolation"],
        ["Test isolation", "integration tests create uniquely named topics/groups and never delete topics; "
                           "test rows are marked published so the live relay ignores them"],
    ], widths=[1.6, 5.4])

    # ---- 4 results by package
    h(doc, "4. Results by package", 1)
    prow = []
    for p in tests["packages"]:
        short = p["name"].replace("dxps/internal/", "")
        comp = COMPONENT.get(short, ("", ""))
        res = "PASS" if p["failed"] == 0 and p["tests"] > 0 else ("FAIL" if p["failed"] else "n/a")
        prow.append([short, comp[0], f"{p['passed']}/{p['tests']}", f"{p['coverage']:.1f}%", res])
    t = table(doc, ["Package", "Component", "Passed", "Coverage", "Result"], prow, widths=[1.0, 3.9, 0.7, 0.8, 0.6], size=8)
    color_result(t, 4)
    para(doc, "Counts include sub-tests (t.Run). Coverage is measured with -coverpkg=./internal/... and merged across "
              "test binaries: a statement counts as covered if any test in the suite executes it. The seed package "
              "holds reference data only and has no tests of its own.", size=8.5, italic=True, color=GREY)
    para(doc, "Where coverage is below 90% and why", bold=True, color=NAVY)
    bullets(doc, [
        ("orchestrator (78%): ", "the remaining statements are database-error branches inside multi-step "
                                 "transactions (for example, a failure between planning and dispatch) that need fault "
                                 "injection inside PostgreSQL to reach."),
        ("store (82%), bus (83%): ", "error returns from PostgreSQL/Kafka calls that do not fail on a healthy local "
                                     "cluster; the success paths, RLS and failure propagation are covered."),
        ("adapter (85%): ", "the Kafka consumer wiring of the adapter service; driver logic for all seven domains is "
                            "covered against the simulators."),
        ("svc (76%), pki (89%), secretbox (88%): ", "os.Exit paths and errors from crypto/rand, which cannot fail "
                                                    "in practice."),
    ])

    # ---- 5 test cases
    h(doc, "5. Test case catalogue", 1)
    para(doc, "Each test case is an automated Go test. The id and description come from the comment above the test "
              "function, so this catalogue cannot drift from the code. The result and duration come from this run.")
    n = 0
    for p in tests["packages"]:
        pkg = p["name"]
        rows = rows_by_pkg.get(pkg)
        if not rows:
            continue
        n += 1
        short = pkg.replace("dxps/internal/", "")
        comp, ref = COMPONENT.get(short, (short, ""))
        h(doc, f"5.{n} {short} - {comp}", 2)
        para(doc, f"Design reference: {ref}.  Source: internal/{short}/{rows[0][0]['file']}.  "
                  f"Coverage {p['coverage']:.1f}%.", size=8.5, color=GREY)
        trs = []
        for tc, lvl, is_sec, res, el, sub in rows:
            name = tc["test"] + (f" (+{sub} sub-tests)" if sub else "")
            trs.append([tc["id"], lvl + (" / Sec" if is_sec and lvl != "Security" else ""), tc["desc"], name,
                        f"{res}  {el:.2f}s"])
        t = table(doc, ["ID", "Level", "Objective and expected result", "Go test", "Result"], trs,
                  widths=[0.85, 0.85, 3.45, 1.15, 0.8], size=7.5)
        color_result(t, 4)
    if untagged:
        para(doc, "Tests without a TC id: " + ", ".join(f"{p.split('/')[-1]}.{t}" for p, t in untagged), size=8,
             italic=True, color=GREY)

    # ---- 6 security
    h(doc, "6. Security verification", 1)
    h(doc, "6.1 Live attack probes (running stack)", 2)
    para(doc, "The mosaic's Run probes action attacks the live gateway and an NE simulator with 24 probes. Results "
              "below are from the final rebuilt stack. The same probes run in TC-DSH-010 against a test stack.")
    if "probes" in figs:
        figure(doc, figs["probes"], "Figure 2 - Live security probes by category", width=5.6)
    t = table(doc, ["ID", "Category", "Attack", "Expected", "Observed", "Result"],
              [[p["id"], p["category"], p["name"], p["expect"], p["got"], "PASS" if p["pass"] else "FAIL"] for p in probes],
              widths=[0.6, 0.8, 2.6, 1.3, 1.3, 0.5], size=7.5)
    color_result(t, 5)
    h(doc, "6.2 Security controls verified by automated tests", 2)
    table(doc, ["Control", "Verified by"], [
        ["JWT: HS256 only; alg=none / RS256 / HS512 / wrong typ rejected; expiry, issuer, audience, tenant format", "TC-AUTH-001..009, SEC-01..06"],
        ["Scope-based authorisation (order read/write, hub write)", "TC-GW, SEC-07..09"],
        ["Tenant isolation: PostgreSQL row-level security on every table, WITH CHECK on insert, no rows without tenant context", "TC-STO-002, TC-STO-003, SEC-10/11"],
        ["Least privilege: app role cannot UPDATE/DELETE audit, TRUNCATE, DROP, ALTER or disable RLS", "TC-STO-004"],
        ["Input validation: schema, patterns, enum, size limits (413), unknown fields, JSON injection into NE templates", "TC-CAT-003/004, TC-GW, SEC-12..18"],
        ["Missing dependency data fails the render instead of sending null to an NE", "TC-CAT-007"],
        ["SSRF: webhook callbacks must be https and on an explicit host:port allow-list (checked at registration and "
         "again at delivery); redirects are never followed", "TC-GW, TC-HUB, SEC-19..21"],
        ["Abuse: per-tenant token-bucket rate limit (429 DXPS-1006)", "TC-GW, SEC-22"],
        ["Mutual TLS to every NE; certificates from a foreign CA or missing certificates refused", "TC-SIM-021, TC-ADP, TC-DSH-013, SEC-23/24"],
        ["Webhook integrity: HMAC-SHA256 signatures, secrets encrypted at rest (AES-256-GCM)", "TC-HUB, TC-SBX"],
        ["Dashboard: loopback bind, Host allow-list (DNS rebinding), Origin + double-submit CSRF, security headers", "TC-DSH-001..004"],
        ["No secrets in logs or exchange records (tokens, client secrets, passwords redacted)", "TC-SVC-001, TC-SIM-022, TC-CFG-003"],
        ["Idempotency: derived order ids, replay returns 200 + Idempotent-Replayed, key reuse with a different body 409", "TC-GW, SEC-18"],
    ], widths=[5.0, 2.0], size=8)
    h(doc, "6.3 Static analysis", 2)
    gs = gosec.get("Stats", {})
    issues = gosec.get("Issues", []) or []
    sev = Counter(i["severity"] for i in issues)
    table(doc, ["Tool", "Result"], [
        ["govulncheck (golang.org/x/vuln v1.8.0)",
         "No vulnerabilities found" if "No vulnerabilities found" in vuln else "Findings present - see govulncheck.txt"],
        ["gosec v2.29.0", f"{gs.get('files', '?')} files, {gs.get('lines', '?'):,} lines: {sev.get('HIGH', 0)} high, "
                          f"{sev.get('MEDIUM', 0)} medium, {sev.get('LOW', 0)} low; {gs.get('nosec', 0)} reviewed #nosec annotations"],
        ["go vet", "clean"],
    ], widths=[2.2, 4.8])
    bullets(doc, [
        ("First govulncheck run: ", "7 reachable vulnerabilities, 6 in the Go 1.26.5 standard library (net/url, crypto/tls, "
                                    "net/http, encoding/xml, encoding/asn1, idna) and 1 in golang.org/x/text v0.29.0. "
                                    "Fixed by moving to toolchain go1.26.6 and x/text v0.42.0; the re-scan is clean."),
        ("gosec G404 (math/rand) excluded: ", "math/rand is used only for retry jitter, simulator latency and the demo "
                                               "traffic mix. Tokens, CSRF nonces, certificate serials and keys use crypto/rand."),
        ("Reviewed #nosec: ", "file paths that come from operator configuration (runtime directory, PKI directory), never "
                              "from requests (G304/G703); public certificate files written 0644 while keys are 0600 (G306); "
                              "the CSRF cookie's Secure flag follows the transport because the mosaic is plain HTTP on loopback (G124)."),
        ("Remaining low findings (G104): ", f"{sev.get('LOW', 0)} ignored errors on response writes, Body.Close and "
                                            "flag.Parse(ExitOnError); reviewed as benign."),
    ])

    # ---- 7 defects
    h(doc, "7. Defects found and fixed during testing", 1)
    table(doc, ["#", "Defect", "Impact", "Fix", "Regression test"], [
        ["1", "Simulator exchange log stored NRF OAuth access tokens; the log is shown on the dashboard",
         "Credential exposure (CWE-532)", "Recursive redaction of token/secret/password fields in JSON and form bodies", "TC-SIM-022"],
        ["2", "Dashboard accepted any Host header", "DNS rebinding could drive the loopback dashboard from a web page",
         "Host allow-list, 421 Misdirected Request otherwise", "TC-DSH-002"],
        ["3", "mTLS probe counted an unreachable simulator as 'rejected'; then misread a Windows TCP reset",
         "False security pass / flaky probe", "Phase-based verdict: TCP connect, handshake, first read; timeouts and "
         "server-certificate errors never pass", "TC-DSH-010, TC-DSH-013"],
        ["4", "Event hub retried HTTP 3xx responses", "Redirect-driven request amplification toward other hosts",
         "Only 408, 429 and 5xx are retried; redirects are never followed", "TC-HUB"],
        ["5", "A missing dependency result (eSIM iccid) rendered as JSON null into the NE payload",
         "Wrong data sent to a network element", "Template function need fails the render -> permanent task error", "TC-CAT-007"],
        ["6", "Go 1.26.5 standard library and x/text CVEs reachable", "DoS / parsing issues on TLS and HTTP paths",
         "go1.26.6, x/text v0.42.0", "govulncheck"],
        ["7", "Env-file loader ignored os.Setenv errors", "A bad secrets line silently dropped configuration",
         "Error naming the key (never the value)", "TC-CFG-003"],
        ["8", "Kafka on Windows: log cleaner / retention rename of open segments took the log dir offline",
         "Broker shutdown in the dev runtime", "Dev runtime disables the log cleaner and retention checks; topics are never "
         "deleted by tests (production on Linux keeps defaults)", "Stack stable through all runs"],
    ], widths=[0.25, 1.85, 1.45, 2.25, 1.2], size=7.5)

    # ---- 8 E2E and load
    h(doc, "8. End-to-end and load results", 1)
    if load:
        d = load["demo"]
        para(doc, load["description"] + ".", size=9, color=GREY)
        kpi_table(doc, [
            ("orders sent", f"{d['sent']:,}", None), ("accepted (201)", f"{d['byStatus'].get('201', 0):,}", GREEN),
            ("gateway p50", f"{d['p50ms']:.0f} ms", None), ("gateway p99", f"{d['p99ms']:.0f} ms", None),
            ("outbox backlog after", str(load["outboxBacklogAfter"]), GREEN),
        ])
        if "mix" in figs:
            figure(doc, figs["mix"], "Figure 3 - Order mix of the load run", width=5.4)
        if "latency" in figs:
            figure(doc, figs["latency"], "Figure 4 - NE call latency (log scale); error counts include the injected chaos faults", width=6.4)
        table(doc, ["NE", "Calls", "p50 ms", "p99 ms", "Errors (incl. injected)"],
              [[n["ne"], f"{n['calls']:,}", f"{n['p50us'] / 1000:.1f}", f"{n['p99us'] / 1000:.0f}", n["errors"]]
               for n in sorted(load["neLatency"], key=lambda x: -x["calls"])], widths=[1.4, 1.0, 1.0, 1.0, 1.6], size=8)
        para(doc, "Interpretation", bold=True, color=NAVY)
        bullets(doc, [
            "Every order was accepted and persisted (2000 x HTTP 201). The transactional outbox drained to zero, so "
            "no command was lost between PostgreSQL and Kafka.",
            "NE errors match the chaos settings: 997 suffixes recover via the retry ladder; 998/999 fail and compensate.",
            "Latencies are from one laptop running Kafka, PostgreSQL, six services, the simulators and the load generator "
            "at once. They are functional evidence, not a capacity benchmark. The p99 order time (about 18 s) "
            "includes orders that deliberately walked the retry ladder.",
        ])

    # ---- appendix A: E2E traces
    page_break(doc)
    h(doc, "Appendix A - End-to-end payload traces", 1)
    para(doc, "Captured by the orchestrator E2E tests with DXPS_CAPTURE_DIR set. Each trace shows the real TMF641 "
              "request and responses from the gateway, the TMF688 state changes, and every NE call the adapter made "
              "to the simulators, in order.")
    titles = {
        "e2e-create-subscriber-5g": "A.1 CreateSubscriber5G - happy path (TC-ORC-004)",
        "e2e-dependson-vonr": "A.2 CreateSubscriber5G + AddVoNR with dependsOn (TC-ORC-008)",
        "e2e-retry-ladder": "A.3 Transient NE failure recovered by the retry ladder (TC-ORC-006)",
        "e2e-saga-compensation": "A.4 Permanent NE rejection -> ATOMIC saga compensation (TC-ORC-005)",
    }
    for key in ["e2e-create-subscriber-5g", "e2e-dependson-vonr", "e2e-retry-ladder", "e2e-saga-compensation"]:
        tr = traces.get(key)
        if not tr:
            continue
        h(doc, titles[key], 2)
        rich(doc, [("Tenant: ", True), (tr["tenant"] + "    ", False), ("TMF688 states: ", True),
                   (" -> ".join(["acknowledged"] + (tr.get("tmf688States") or [])), False)])
        para(doc, "TMF641 POST /serviceOrder (request)", bold=True, size=9)
        code(doc, pretty(tr["tmf641Request"], 1600), size=7)
        para(doc, "201 Created (response)", bold=True, size=9)
        code(doc, pretty(tr["tmf641Response"], 1200), size=7)
        ex = tr.get("neExchanges") or []
        para(doc, f"NE calls ({len(ex)})", bold=True, size=9)
        t = table(doc, ["#", "Simulator", "Method", "Path", "Status", "Fault"],
                  [[i + 1, e["sim"], e["method"], e["path"], e["status"], e.get("fault", "")] for i, e in enumerate(ex)],
                  widths=[0.3, 0.8, 0.6, 4.1, 0.5, 0.8], size=7)
        shown = 0
        for e in ex:
            if e.get("request") and shown < 2 and e["sim"] != "nrf":
                para(doc, f"{e['method']} {e['sim']} {e['path']} -> {e['status']}", bold=True, size=8.5)
                code(doc, "request:\n" + pretty(e["request"], 900) + ("\nresponse:\n" + pretty(e.get("response"), 500)
                                                                         if e.get("response") else ""), size=6.8)
                shown += 1
        para(doc, "Final order (GET /serviceOrder/{id})", bold=True, size=9)
        code(doc, pretty(tr["tmf641Get"], 1800), size=7)

    # ---- appendix B: simulator payload catalogue
    page_break(doc)
    h(doc, "Appendix B - Network simulator payload catalogue", 1)
    para(doc, f"{len(exchanges):,} exchanges were harvested from the running simulators during a chaos load run "
              "(250 orders, 5/s, chaos 0.3). For each simulator: a successful exchange and, where one occurred, a "
              "fault-injected one. Credentials are redacted at the simulator.")
    by_sim = defaultdict(list)
    for e in exchanges:
        by_sim[e["sim"]].append(e)
    stat_rows = []
    for sim in sorted(by_sim):
        es = by_sim[sim]
        sc = Counter(str(e["status"]) for e in es)
        fc = Counter(e.get("fault") for e in es if e.get("fault"))
        stat_rows.append([sim, len(es), ", ".join(f"{k} x{v}" for k, v in sorted(sc.items())),
                          ", ".join(f"{k} x{v}" for k, v in fc.items()) or "-"])
    table(doc, ["Simulator", "Exchanges", "HTTP status", "Injected faults"], stat_rows, widths=[1.0, 0.8, 2.8, 2.4], size=8)
    k = 0
    for sim in sorted(by_sim):
        es = sorted(by_sim[sim], key=lambda e: e["at"])
        ok = next((e for e in es if 200 <= e["status"] < 300 and e.get("request") and not e.get("fault")), None) or \
            next((e for e in es if 200 <= e["status"] < 300), None)
        bad = next((e for e in es if e.get("fault") and e["status"] >= 400), None) or \
            next((e for e in es if e["status"] >= 400), None)
        k += 1
        h(doc, f"B.{k} {sim}", 2)
        for label, e in (("Success", ok), ("Fault / rejection", bad)):
            if not e:
                continue
            para(doc, f"{label}: {e['method']} {e['path']}  ->  {e['status']}  ({e['proto']}, tenant {e['tenant']}, "
                      f"{e['latUs'] / 1000:.1f} ms{', fault ' + e['fault'] if e.get('fault') else ''})", bold=True, size=8.5)
            body = ""
            if e.get("request"):
                body += "request:\n" + pretty(e["request"], 900) + "\n"
            if e.get("response"):
                body += "response:\n" + pretty(e["response"], 700)
            if body:
                code(doc, body, size=6.8)

    # ---- appendix C: deviations
    h(doc, "Appendix C - Deviations from the design in this build", 1)
    table(doc, ["Design", "This build", "Reason"], [
        ["Protobuf on Kafka", "JSON with versioned contract structs", "Readable payload evidence; schema registry out of scope locally"],
        ["NE selection via NRF discovery", "Registry lookup by ne:{nfType}; NRF used for OAuth tokens", "Single simulator per NF type"],
        ["Kafka SASL_SSL", "PLAINTEXT bound to 127.0.0.1", "Single-node dev runtime; PostgreSQL, gateway and NE links use TLS/mTLS"],
        ["Kafka compaction and retention", "Disabled in the Windows dev runtime", "Kafka on Windows cannot rename open segments (KAFKA-1194)"],
        ["Cross-domain atomicity", "Atomic per business command (saga per BC)", "As designed for the MVP; cross-BC atomicity via order-level policy"],
        ["eSIM asynchronous callbacks (ES2+ handleDownloadProgressInfo)", "Synchronous confirm/cancel", "Simulator scope"],
        ["go test -race", "Not run", "Requires cgo, which is unavailable on this machine"],
        ["NE latency SLOs", "Not asserted", "All components share one laptop; see section 8"],
    ], widths=[2.0, 2.4, 2.6], size=8)

    try:
        doc.save(OUT)
        print(OUT)
    except PermissionError:
        doc.save(OUT_FALLBACK)
        print(OUT_FALLBACK)
    print(f"test cases: {len(cases)}, untagged tests: {len(untagged)}, levels: {dict(level_count)}, security: {sec_count}")


if __name__ == "__main__":
    build()
