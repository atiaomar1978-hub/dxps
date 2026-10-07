"use strict";
// DxPS Mosaic. All DOM is built with textContent / setAttribute (no innerHTML of data) - payloads are untrusted.
(() => {
  const $ = (id) => document.getElementById(id);
  const SVG = "http://www.w3.org/2000/svg";
  let csrf = "", snap = null, selected = null, lastOk = 0;

  function h(tag, attrs, ...kids) {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v == null || v === false) continue;
      if (k === "class") el.className = v;
      else if (k === "style") for (const [p, x] of Object.entries(v)) { if (p.startsWith("--")) el.style.setProperty(p, x); else el.style[p] = x; }
      else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, v);
    }
    for (const c of kids.flat()) if (c != null && c !== false) el.append(c instanceof Node ? c : document.createTextNode(String(c)));
    return el;
  }
  function s(tag, attrs, ...kids) {
    const el = document.createElementNS(SVG, tag);
    for (const [k, v] of Object.entries(attrs || {})) if (v != null) el.setAttribute(k, v);
    for (const c of kids.flat()) if (c != null) el.append(c instanceof Node ? c : document.createTextNode(String(c)));
    return el;
  }
  const body = (id) => $(id).querySelector(".body");
  const fill = (el, ...kids) => { el.replaceChildren(...kids.flat().filter(Boolean)); };
  const fmt = (n, d = 0) => (n == null || isNaN(n)) ? "-" : Number(n).toLocaleString(undefined, { maximumFractionDigits: d, minimumFractionDigits: d });
  const ms = (us) => us == null ? "-" : us >= 1000 ? fmt(us / 1000, us < 10000 ? 1 : 0) + " ms" : fmt(us) + " µs";
  const ago = (t) => { const d = (Date.now() - new Date(t)) / 1000; return d < 60 ? fmt(d) + "s" : d < 3600 ? fmt(d / 60) + "m" : fmt(d / 3600) + "h"; };
  const palette = ["var(--c1)", "var(--c2)", "var(--c3)", "var(--c4)", "var(--c5)", "var(--c6)", "var(--c7)"];
  const stateColor = { completed: "var(--ok)", failed: "var(--bad)", rejected: "#c2335a", inProgress: "var(--info)", acknowledged: "#3b6fd1",
    partial: "var(--warn)", cancelled: "var(--c2)", held: "#b98b2c", pending: "#7c8bb0", assessingCancellation: "#a77" };
  const tenantColor = {}; const tcol = (t) => tenantColor[t] || (tenantColor[t] = palette[Object.keys(tenantColor).length % palette.length]);

  // ------------------------------------------------------------------ API
  async function session() {
    const r = await fetch("/api/session", { credentials: "same-origin" });
    csrf = (await r.json()).csrf;
  }
  async function post(path, payload) {
    const r = await fetch(path, { method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf }, body: JSON.stringify(payload || {}) });
    if (r.status === 403) { await session(); }
    return r;
  }
  async function tick() {
    try {
      const r = await fetch("/api/snapshot", { credentials: "same-origin" });
      snap = await r.json(); lastOk = Date.now();
      render();
    } catch (e) { $("age").textContent = "dashboard unreachable"; }
  }

  // ------------------------------------------------------------------ header & KPIs
  function renderHeader() {
    const names = ["gateway", "orchestrator", "adapter", "eventhub", "netsim", "postgres", "kafka"];
    fill($("svc"), names.map((n) => h("span", { class: "chip " + (snap.up[n] ? "up" : "down"), title: (snap.errors || {})[n] || "healthy" }, h("i"), n)));
    $("clock").textContent = new Date(snap.at).toLocaleTimeString();
  }
  function kpi(label, value, sub, accent) {
    return h("div", { class: "kpi", style: { "--accent": accent } }, h("span", {}, label), h("b", {}, value), h("small", {}, sub || ""));
  }
  function renderKPIs() {
    const db = snap.db || {}, hub = snap.eventhub || {};
    const lag = (snap.lags || []).reduce((a, l) => a + Math.max(0, l.lag), 0);
    const total = (db.ordersByState || []).reduce((a, c) => a + c.n, 0);
    const done = (db.ordersByState || []).filter((c) => c.key === "completed").reduce((a, c) => a + c.n, 0);
    const probes = snap.probes || [];
    const tests = snap.tests || {};
    fill($("kpis"),
      kpi("Orders / min", fmt(db.ordersLastMin), fmt(total) + " total", "var(--c1)"),
      kpi("NE tasks / min", fmt(db.tasksLastMin), "southbound calls", "var(--c2)"),
      kpi("Order p99", db.p99OrderMs ? fmt(db.p99OrderMs) + " ms" : "-", "last 5 min, end-to-end", "var(--c4)"),
      kpi("Success", total ? fmt(100 * done / total, 1) + "%" : "-", fmt(done) + " completed", "var(--ok)"),
      kpi("Consumer lag", fmt(lag), (snap.lags || []).length + " groups", lag > 1000 ? "var(--bad)" : "var(--c3)"),
      kpi("Outbox backlog", fmt(db.outboxBacklog), "unpublished rows", (db.outboxBacklog || 0) > 100 ? "var(--warn)" : "var(--c6)"),
      kpi("Webhooks", fmt(hub.delivered), fmt(hub.failed) + " failed", "var(--c5)"),
      kpi("Security", probes.length ? probes.filter((p) => p.pass).length + "/" + probes.length : "-",
        tests.totals ? fmt(tests.totals.passed) + " tests passed" : "probes passed", "var(--c7)"));
  }

  // ------------------------------------------------------------------ charts
  function spark(points, key, w, hgt, color, area) {
    const vals = points.map((p) => p[key] || 0), mx = Math.max(1, ...vals);
    const xs = (i) => (i / Math.max(1, vals.length - 1)) * w, ys = (v) => hgt - 4 - (v / mx) * (hgt - 8);
    const d = vals.map((v, i) => (i ? "L" : "M") + xs(i).toFixed(1) + "," + ys(v).toFixed(1)).join("");
    const g = s("g");
    if (area && vals.length > 1) g.append(s("path", { d: d + `L${w},${hgt}L0,${hgt}Z`, fill: color, opacity: ".12" }));
    g.append(s("path", { d: d || "M0,0", fill: "none", stroke: color, "stroke-width": "2", "stroke-linejoin": "round" }));
    return g;
  }
  function renderThroughput() {
    const el = body("t-throughput"), hist = snap.history || [];
    const W = Math.max(300, el.clientWidth), H = Math.max(120, el.clientHeight - 30);
    const svg = s("svg", { width: W, height: H, viewBox: `0 0 ${W} ${H}` });
    for (let i = 1; i < 4; i++) svg.append(s("line", { x1: 0, x2: W, y1: (H * i) / 4, y2: (H * i) / 4, stroke: "rgba(120,160,255,.07)" }));
    svg.append(spark(hist, "tasks", W, H, "#8f7bff", true), spark(hist, "orders", W, H, "#3fd0ff", true), spark(hist, "lag", W, H, "#ff5470", false));
    const last = hist[hist.length - 1] || {};
    fill(el, svg, h("div", { class: "legend" },
      h("span", {}, h("i", { style: { background: "#3fd0ff" } }), "orders/min ", h("b", {}, fmt(last.orders))),
      h("span", {}, h("i", { style: { background: "#8f7bff" } }), "tasks/min ", h("b", {}, fmt(last.tasks))),
      h("span", {}, h("i", { style: { background: "#ff5470" } }), "consumer lag ", h("b", {}, fmt(last.lag))),
      h("span", {}, "p99 ", h("b", {}, last.p99 ? fmt(last.p99) + " ms" : "-"))));
  }
  function donut(parts, size) {
    const tot = parts.reduce((a, p) => a + p.v, 0) || 1, r = size / 2 - 10, c = 2 * Math.PI * r;
    const svg = s("svg", { width: size, height: size, viewBox: `0 0 ${size} ${size}` });
    let off = 0;
    svg.append(s("circle", { cx: size / 2, cy: size / 2, r, fill: "none", stroke: "rgba(255,255,255,.05)", "stroke-width": 16 }));
    for (const p of parts) {
      const len = (p.v / tot) * c;
      svg.append(s("circle", { cx: size / 2, cy: size / 2, r, fill: "none", stroke: p.c, "stroke-width": 16,
        "stroke-dasharray": `${len} ${c - len}`, "stroke-dashoffset": -off, transform: `rotate(-90 ${size / 2} ${size / 2})` }, s("title", {}, p.k + ": " + p.v)));
      off += len;
    }
    svg.append(s("text", { x: size / 2, y: size / 2 - 2, "text-anchor": "middle", style: null, "font-size": 22, fill: "#dfe8ff" }, fmt(tot)));
    svg.append(s("text", { x: size / 2, y: size / 2 + 16, "text-anchor": "middle" }, "orders"));
    return svg;
  }
  function renderStates() {
    const by = {};
    for (const c of (snap.db || {}).ordersByState || []) by[c.key] = (by[c.key] || 0) + c.n;
    const parts = Object.entries(by).sort((a, b) => b[1] - a[1]).map(([k, v]) => ({ k, v, c: stateColor[k] || "var(--dim)" }));
    const el = body("t-states");
    if (!parts.length) return fill(el, h("div", { class: "empty" }, "no orders yet - start the load generator"));
    fill(el, h("div", { style: { display: "flex", justifyContent: "center" } }, donut(parts, Math.min(170, el.clientWidth))),
      h("div", { class: "legend" }, parts.map((p) => h("span", {}, h("i", { style: { background: p.c } }), p.k + " " + fmt(p.v)))));
  }

  // ------------------------------------------------------------------ tenants & lanes
  function renderTenants() {
    const orch = snap.orchestrator || {}, by = (orch.bcScheduler || {}).byTenant || {};
    const served = Object.values(by).reduce((a, b) => a + b, 0) || 1;
    const states = {};
    for (const c of (snap.db || {}).ordersByState || []) (states[c.tenant] = states[c.tenant] || {})[c.key] = c.n;
    fill(body("t-tenants"), h("div", { class: "tenants" }, (snap.tenants || []).map((x) => {
      const t = { ...x, id: x.tenant, type: x.tenantType || "" };
      const st = states[t.id] || {}, tot = Object.values(st).reduce((a, b) => a + b, 0);
      const share = (by[t.id] || 0) / served;
      return h("div", { class: "tcard" + (t.status !== "ACTIVE" ? " suspended" : "") },
        h("div", { style: { display: "flex", justifyContent: "space-between", alignItems: "center" } },
          h("h3", { style: { color: tcol(t.id) } }, t.id), h("span", { class: "badge " + (t.status !== "ACTIVE" ? "SUSPENDED" : t.type) }, t.status !== "ACTIVE" ? t.status : t.type.replace("_", " "))),
        h("div", { class: "meta" }, t.name),
        h("div", { class: "bar" }, h("span", { style: { width: (share * 100).toFixed(1) + "%", background: tcol(t.id) } })),
        h("div", { class: "kv" }, h("span", {}, "DRR share"), h("b", {}, fmt(share * 100, 1) + "%")),
        h("div", { class: "kv" }, h("span", {}, "SLA weight / quota"), h("b", {}, t.slaWeight + " / " + fmt(t.quotaTps) + " tps")),
        h("div", { class: "bar", style: { marginTop: "8px" } }, Object.entries(st).map(([k, n]) =>
          h("span", { title: k + " " + n, style: { width: (100 * n / (tot || 1)) + "%", background: stateColor[k] || "var(--dim)" } }))),
        h("div", { class: "kv" }, h("span", {}, "orders"), h("b", {}, fmt(tot))));
    })));
  }
  function renderLanes() {
    const o = snap.orchestrator || {}, sch = o.bcScheduler || { queued: [0, 0, 0, 0], served: [0, 0, 0, 0] }, ln = o.bcLanes || {}, rl = o.resultLanes || {};
    const mx = Math.max(1, ...sch.served);
    const names = ["P0 preempt", "P1 real-time", "P2 standard", "P3 bulk"], w = ["strict", "4", "2", "1"];
    fill(body("t-lanes"), h("div", { class: "lanes" }, [0, 1, 2, 3].map((i) =>
      h("div", { class: "lane" }, h("div", { class: `tag p${i} tagp${i}` }, "P" + i),
        h("div", { class: "flow", title: names[i] }, h("span", { class: "bgp" + i, style: { width: (100 * sch.served[i] / mx) + "%", opacity: ".55" } }),
          h("em", {}, names[i] + " · w " + w[i])),
        h("div", { class: "num" }, fmt(sch.served[i]) + " served", h("br"), fmt(sch.queued[i]) + " queued")))),
      h("div", { class: "minis" },
        h("div", { class: "mini" }, h("span", {}, "key lanes"), h("b", {}, fmt(ln.Lanes))),
        h("div", { class: "mini" }, h("span", {}, "in flight"), h("b", {}, fmt((ln.InFlight || 0) + (rl.InFlight || 0)))),
        h("div", { class: "mini" }, h("span", {}, "max lane depth"), h("b", {}, fmt(Math.max(ln.MaxLaneDepth || 0, rl.MaxLaneDepth || 0))))));
  }

  // ------------------------------------------------------------------ NEs
  const simOf = { nrf01: "nrf", udr01: "udr", imspg01: "ims-pg", pe01: "restconf", olt01: "restconf", usp01: "restconf", smdp01: "smdp",
    ocs01: "ocs-bss", "ocs-alpha": "ocs-bss", npdb01: "npdb", nef01: "nef" };
  function renderNEs() {
    const ad = Array.isArray(snap.adapter) ? snap.adapter : [], sims = Array.isArray(snap.sims) ? snap.sims : [];
    const simBy = Object.fromEntries(sims.map((x) => [x.name, x]));
    const ex = Array.isArray(snap.exchanges) ? snap.exchanges : [];
    const series = {};
    for (const e of ex) (series[e.sim] = series[e.sim] || []).push({ v: e.latUs });
    const neTiles = ad.map((n) => {
      const id = n.ne.split("/").pop(), sim = simBy[simOf[id]] || {};
      const cls = "ne" + (n.breaker === "open" ? " open" : n.breaker === "half-open" ? " half" : "") + (sim.errorRate > 0 || sim.latencyMs > 50 ? " faulty" : "");
      return h("div", { class: cls, onclick: () => openFault(simOf[id] || id), title: "click to inject faults into " + (simOf[id] || id) },
        h("h4", {}, id, h("span", { class: "dot" })), h("div", { class: "sub" }, n.ne + " · " + (n.breaker || "closed")),
        h("div", { class: "stats" }, h("span", {}, "calls ", h("b", {}, fmt(n.calls))), h("span", {}, "errors ", h("b", {}, fmt(n.errors))),
          h("span", {}, "avg ", h("b", {}, ms(n.calls ? n.totalUs / n.calls : null))), h("span", {}, "last ", h("b", {}, ms(n.lastUs)))));
    });
    const simTiles = sims.map((x) => {
      const sv = series[x.name] || [], W = 130, H = 26;
      const svg = s("svg", { width: W, height: H, class: "spark" });
      if (sv.length > 1) svg.append(spark(sv, "v", W, H, x.errorRate > 0 ? "#ff5470" : "#2ee6a6", true));
      const errs = Object.entries(x.byStatus || {}).filter(([k]) => k >= 400).reduce((a, [, v]) => a + v, 0);
      return h("div", { class: "ne" + (x.errorRate > 0 || x.latencyMs > 50 ? " faulty" : ""), onclick: () => openFault(x.name) },
        h("h4", {}, x.name, h("span", { class: "sub" }, ":" + x.port)),
        h("div", { class: "stats" }, h("span", {}, "calls ", h("b", {}, fmt(x.calls))), h("span", {}, "4xx/5xx ", h("b", {}, fmt(errs))),
          h("span", {}, "p50 ", h("b", {}, ms(x.p50us))), h("span", {}, "p99 ", h("b", {}, ms(x.p99us))),
          h("span", {}, "objects ", h("b", {}, fmt(x.objects))), h("span", {}, "faults ", h("b", {}, fmt(x.faults)))),
        x.errorRate > 0 || x.latencyMs > 50 ? h("div", { class: "sub", style: { color: "var(--bad)" } }, `injected: +${x.latencyMs}ms, ${fmt(x.errorRate * 100)}% errors`) : null, svg);
    });
    fill(body("t-ne"), h("div", { class: "sectionlabel" }, "Adapter view - circuit breakers & latency per NE"),
      neTiles.length ? h("div", { class: "nes" }, neTiles) : h("div", { class: "empty" }, "adapter not reporting yet"),
      h("div", { class: "sectionlabel" }, "Simulator view - what the network sees (mTLS, HTTP/2)"), h("div", { class: "nes" }, simTiles));
  }
  function renderDomains() {
    const by = {};
    for (const c of (snap.db || {}).tasksByDomain || []) { const d = (by[c.key] = by[c.key] || {}); d[c.sub || "?"] = (d[c.sub || "?"] || 0) + c.n; }
    const col = { SUCCEEDED: "var(--ok)", FAILED: "var(--bad)", COMPENSATED: "var(--c2)", READY: "var(--info)", BLOCKED: "var(--warn)", SKIPPED: "var(--dim)" };
    const rows = Object.entries(by).sort((a, b) => Object.values(b[1]).reduce((x, y) => x + y, 0) - Object.values(a[1]).reduce((x, y) => x + y, 0));
    const mx = Math.max(1, ...rows.map(([, v]) => Object.values(v).reduce((a, b) => a + b, 0)));
    const el = body("t-domains");
    if (!rows.length) return fill(el, h("div", { class: "empty" }, "no NE tasks yet"));
    fill(el, rows.map(([d, st]) => {
      const tot = Object.values(st).reduce((a, b) => a + b, 0);
      return h("div", { style: { marginBottom: "10px" } }, h("div", { class: "kv" }, h("span", { style: { color: "var(--tx)" } }, d), h("b", {}, fmt(tot))),
        h("div", { class: "bar", style: { height: "10px", width: (100 * tot / mx) + "%" } },
          Object.entries(st).map(([k, n]) => h("span", { title: k + " " + n, style: { width: (100 * n / tot) + "%", background: col[k] || "var(--dim)" } }))));
    }), h("div", { class: "legend" }, Object.entries(col).map(([k, c]) => h("span", {}, h("i", { style: { background: c } }), k.toLowerCase()))));
  }

  // ------------------------------------------------------------------ payload feed
  function jsonView(label, raw) {
    const out = [h("span", { class: "h" }, label + "\n")];
    let v = raw;
    if (typeof v === "string") { try { v = JSON.parse(v); } catch { out.push(v + "\n"); return out; } }
    if (v == null) { out.push(h("span", { class: "dimtx" }, "(empty)\n")); return out; }
    const txt = JSON.stringify(v, null, 2);
    const re = /("(?:\\.|[^"\\])*")(\s*:)?|\b(true|false|null)\b|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)/g;
    let i = 0, m;
    while ((m = re.exec(txt))) {
      out.push(txt.slice(i, m.index));
      if (m[1]) out.push(h("span", { class: m[2] ? "k" : "s" }, m[1]), m[2] || "");
      else if (m[3]) out.push(h("span", { class: "b" }, m[3]));
      else out.push(h("span", { class: "n" }, m[4]));
      i = re.lastIndex;
    }
    out.push(txt.slice(i) + "\n\n");
    return out;
  }
  function showExchange(e) {
    selected = e.at + e.path;
    fill($("json"), h("span", { class: "h" }, `${e.method} ${e.path}  ${e.proto}\n`),
      h("span", { class: "dimtx" }, `simulator ${e.sim} · tenant ${e.tenant || "-"} · ${e.status} · ${ms(e.latUs)}${e.fault ? " · fault " + e.fault : ""}\n\n`),
      jsonView("REQUEST", e.request), jsonView("RESPONSE", e.response));
    document.querySelectorAll(".xrow").forEach((r) => r.classList.toggle("sel", r.dataset.k === selected));
  }
  function renderFeed() {
    const ex = (Array.isArray(snap.exchanges) ? snap.exchanges : []).slice().reverse().slice(0, 60);
    if (!ex.length) return fill($("feed"), h("div", { class: "empty" }, "no southbound traffic yet"));
    fill($("feed"), ex.map((e) => h("div", { class: "xrow" + (selected === e.at + e.path ? " sel" : ""), "data-k": e.at + e.path, onclick: () => showExchange(e) },
      h("span", { class: "dimtx" }, new Date(e.at).toLocaleTimeString()), h("span", { style: { color: tcol(e.tenant) } }, e.sim),
      h("span", { class: "m " + e.method }, e.method), h("span", { class: "path", title: e.path }, e.path),
      h("span", { class: "st s" + String(e.status)[0] }, e.status), h("span", { class: "dimtx" }, ms(e.latUs)))));
    if (!selected) showExchange(ex[0]);
  }

  // ------------------------------------------------------------------ topics
  function renderTopics() {
    const ts = (snap.topics || []).slice().sort((a, b) => a.topic.localeCompare(b.topic));
    const mx = Math.log10(Math.max(10, ...ts.map((t) => t.end)));
    const color = (t) => {
      const k = t.end ? Math.log10(t.end + 1) / mx : 0;
      const hue = t.topic.includes("dlq") ? 350 : t.topic.includes("retry") ? 35 : t.topic.includes(".p0") ? 345 : t.topic.includes("state") ? 160 : 215 + 40 * k;
      return `hsla(${hue}, 80%, ${18 + 32 * k}%, ${0.35 + 0.6 * k})`;
    };
    fill(body("t-topics"), h("div", { class: "heat" }, ts.map((t) => h("div", { class: "cell", title: t.topic, style: { background: color(t) } },
      h("span", {}, t.topic.replace("dxps.", "")), h("b", {}, fmt(t.end))))),
      h("div", { class: "sectionlabel" }, "consumer groups"),
      (snap.lags || []).sort((a, b) => b.lag - a.lag).slice(0, 30).map((l) => h("div", { class: "lag" },
        h("span", {}, l.group.replace("dxps-", "")), h("span", { class: "dimtx" }, l.state), h("span", { class: "n", style: { color: l.lag > 100 ? "var(--warn)" : "var(--ok)" } }, fmt(l.lag)))));
  }

  // ------------------------------------------------------------------ orders, hooks, security, tests, catalog
  function renderOrders() {
    const rec = (snap.db || {}).recent || [];
    if (!rec.length) return fill(body("t-orders"), h("div", { class: "empty" }, "no orders yet"));
    fill(body("t-orders"), h("table", {}, h("thead", {}, h("tr", {}, ["tenant", "external id", "spec", "prio", "state", "duration", "age"].map((x) => h("th", {}, x)))),
      h("tbody", {}, rec.map((o) => h("tr", {}, h("td", { style: { color: tcol(o.tenant) } }, o.tenant), h("td", { class: "mono", title: o.id }, o.externalId || o.id.slice(0, 8)),
        h("td", {}, o.spec), h("td", { class: "p" + o.priority }, "P" + o.priority), h("td", {}, h("span", { class: "state " + o.state }, o.state)),
        h("td", {}, o.durationMs ? fmt(o.durationMs) + " ms" : "-"), h("td", { class: "dimtx" }, ago(o.at)))))));
  }
  function renderHooks() {
    const hub = snap.eventhub || {}, rec = (hub.recent || []).slice().reverse();
    fill(body("t-hooks"), h("div", { class: "tot" }, h("div", {}, h("b", { style: { color: "var(--ok)" } }, fmt(hub.delivered)), "delivered"),
      h("div", {}, h("b", { style: { color: "var(--bad)" } }, fmt(hub.failed)), "failed")),
      h("div", { class: "hooks" }, rec.length ? rec.map((d) => h("div", { class: "h" }, h("span", { style: { color: tcol(d.tenant) } }, d.tenant),
        h("span", {}, h("span", { class: "state " + d.state }, d.state)), h("span", { class: "st s" + String(d.status)[0] }, d.status > 0 ? d.status : "blocked"),
        h("span", { class: "dimtx" }, "x" + d.attempts))) : h("div", { class: "empty" }, "no deliveries yet")));
  }
  function renderSecurity() {
    const ps = snap.probes || [], pass = ps.filter((p) => p.pass).length;
    $("b-probe").disabled = !!(snap.demo || {}).probing;
    const posture = ["TLS 1.3 only", "mTLS to NEs", "JWT HS256 strict", "RLS forced", "SCRAM-SHA-256", "HMAC webhooks", "AES-256-GCM secrets", "CSP strict", "CSRF tokens"];
    fill(body("t-security"), h("div", { class: "scorebox" },
      h("div", { class: "score", style: { color: ps.length && pass === ps.length ? "var(--ok)" : ps.length ? "var(--bad)" : "var(--dim)" } }, ps.length ? `${pass}/${ps.length}` : "-"),
      h("div", { class: "posture" }, posture.map((p) => h("span", { class: "chip up" }, h("i"), p)))),
      ps.length ? h("div", { class: "probes" }, ps.map((p) => h("div", { class: "probe", title: "expected " + p.expect },
        h("span", { class: "ic " + (p.pass ? "ok" : "no") }, p.pass ? "✓" : "✗"),
        h("span", {}, h("span", { class: "cat" }, p.id + " " + p.category), p.name), h("span", { class: "got" }, p.got))))
        : h("div", { class: "empty" }, (snap.demo || {}).probing ? "running probes..." : "press Run probes to attack the live system"));
  }
  function renderTests() {
    const t = snap.tests;
    if (!t || !t.packages) return fill(body("t-tests"), h("div", { class: "empty" }, "run scripts\\test-all.ps1 to publish results"));
    fill(body("t-tests"), h("div", { class: "tot" },
      h("div", {}, h("b", {}, fmt(t.totals.tests)), "tests"), h("div", {}, h("b", { style: { color: "var(--ok)" } }, fmt(t.totals.passed)), "passed"),
      h("div", {}, h("b", { style: { color: t.totals.failed ? "var(--bad)" : "var(--dim)" } }, fmt(t.totals.failed)), "failed"),
      h("div", {}, h("b", { style: { color: "var(--c4)" } }, fmt(t.coverage, 1) + "%"), "coverage")),
      h("div", { class: "tests" }, t.packages.map((p) => h("div", { class: "pkg" }, h("span", { title: p.name, style: { color: p.failed ? "var(--bad)" : "var(--tx)" } }, p.name.replace("dxps/internal/", "")),
        h("span", { class: "dimtx" }, p.passed + "/" + p.tests),
        h("div", { class: "bar", title: fmt(p.coverage, 1) + "%" }, h("span", { style: { width: p.coverage + "%", background: p.coverage >= 80 ? "var(--ok)" : p.coverage >= 60 ? "var(--warn)" : "var(--bad)" } }))))));
  }
  function renderCatalog() {
    fill(body("t-catalog"), h("div", { class: "cat" }, (snap.catalog || []).map((c) => h("div", { class: "spec" + (c.tenant !== "GLOBAL" ? " ovr" : "") },
      h("span", {}, c.spec, h("span", { class: "dimtx" }, " " + c.version + (c.tenant !== "GLOBAL" ? " · " + c.tenant : ""))),
      h("span", { class: c.priority }, c.priority.toUpperCase()), h("span", { class: "dimtx" }, c.txMode + " · " + c.tasks)))));
  }
  function renderLoad() {
    const d = snap.demo || {};
    $("b-load").disabled = !!d.running;
    $("b-load").textContent = d.running ? "Running..." : "Start load";
    const pct = d.target ? Math.min(100, 100 * d.sent / d.target) : 0;
    fill($("loadstat"), d.target ? [
      h("div", { class: "kv" }, h("span", {}, "sent"), h("b", {}, fmt(d.sent) + " / " + fmt(d.target))),
      h("div", { class: "bar" }, h("span", { style: { width: pct + "%", background: "linear-gradient(90deg,#2a7dff,#8f5bff)" } })),
      h("div", { class: "kv" }, h("span", {}, "gateway p50 / p99"), h("b", {}, fmt(d.p50ms, 1) + " / " + fmt(d.p99ms, 1) + " ms")),
      h("div", { class: "codes" }, Object.entries(d.byStatus || {}).map(([k, v]) => h("span", { class: "chip" }, h("span", { class: "st s" + k[0] }, k), "×" + fmt(v))),
        Object.entries(d.byCode || {}).map(([k, v]) => h("span", { class: "chip" }, k + " ×" + fmt(v)))),
      d.lastError ? h("div", { style: { color: "var(--bad)", marginTop: "6px" } }, d.lastError) : null] :
      h("div", {}, "Generates a realistic mix across tenants: 5G subscriber create (+VoNR), eSIM, plan change, P0 fraud suspend, port-in, FTTH, L3VPN, QoD. Chaos injects NE faults via MSISDN/EID suffixes (997 transient, 998 reject, 999 down)."));
  }

  function render() {
    renderHeader(); renderKPIs(); renderThroughput(); renderStates(); renderTenants(); renderLanes(); renderNEs(); renderDomains();
    renderFeed(); renderTopics(); renderOrders(); renderHooks(); renderSecurity(); renderTests(); renderCatalog(); renderLoad();
  }

  // ------------------------------------------------------------------ controls
  let faultSim = "";
  function openFault(sim) {
    faultSim = sim;
    const x = (Array.isArray(snap.sims) ? snap.sims : []).find((y) => y.name === sim) || {};
    $("fault-title").textContent = "Inject fault into " + sim;
    $("fd-lat").value = x.latencyMs || 0; $("fd-err").value = x.errorRate || 0;
    $("fo-lat").textContent = $("fd-lat").value + " ms"; $("fo-err").textContent = Math.round($("fd-err").value * 100) + "%";
    $("fault").showModal();
  }
  function wire() {
    $("f-chaos").addEventListener("input", () => { $("o-chaos").textContent = Math.round($("f-chaos").value * 100) + "%"; });
    $("fd-lat").addEventListener("input", () => { $("fo-lat").textContent = $("fd-lat").value + " ms"; });
    $("fd-err").addEventListener("input", () => { $("fo-err").textContent = Math.round($("fd-err").value * 100) + "%"; });
    $("loadform").addEventListener("submit", async (e) => {
      e.preventDefault();
      await post("/api/demo", { count: +$("f-count").value, rate: +$("f-rate").value, chaos: +$("f-chaos").value });
      tick();
    });
    $("b-probe").addEventListener("click", async () => { await post("/api/probe", {}); tick(); });
    $("fault").addEventListener("close", async () => {
      const v = $("fault").returnValue;
      if (v === "apply") await post("/api/fault", { sim: faultSim, latencyMs: +$("fd-lat").value, errorRate: +$("fd-err").value });
      if (v === "reset") await post("/api/fault", { sim: faultSim, latencyMs: 0, errorRate: 0 });
      tick();
    });
    setInterval(() => { $("age").textContent = lastOk ? "updated " + ((Date.now() - lastOk) / 1000).toFixed(1) + "s ago" : "connecting..."; }, 250);
    window.addEventListener("resize", () => snap && renderThroughput());
  }

  wire();
  session().then(tick);
  setInterval(tick, 2000);
})();
