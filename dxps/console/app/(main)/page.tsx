"use client";

import Link from "next/link";
import { Badge, Card, Dot, ErrorBox, Page, StackBar, Stat, Table } from "@/components/ui";
import { fmtMs, fmtNum, fmtTime, fmtUs, pct, prio, toneOf } from "@/lib/format";
import { useApi } from "@/lib/hooks";
import type { Count, Overview } from "@/lib/types";

const services = ["gateway", "orchestrator", "adapter", "eventhub", "netsim", "kafka", "mosaic"];

function byTenant(rows: Count[]): Map<string, Map<string, number>> {
  const m = new Map<string, Map<string, number>>();
  for (const r of rows) {
    const t = m.get(r.tenant) ?? new Map<string, number>();
    t.set(r.key, (t.get(r.key) ?? 0) + r.n);
    m.set(r.tenant, t);
  }
  return m;
}

export default function Dashboard() {
  const o = useApi<Overview>("overview", 5000);
  const d = o.data;
  const db = d?.db;
  const states = byTenant(db?.ordersByState ?? []);
  const totals = new Map<string, number>();
  for (const r of db?.ordersByState ?? []) totals.set(r.key, (totals.get(r.key) ?? 0) + r.n);
  const all = [...totals.values()].reduce((a, b) => a + b, 0);
  const active = (totals.get("acknowledged") ?? 0) + (totals.get("inProgress") ?? 0) + (totals.get("pending") ?? 0) + (totals.get("held") ?? 0);
  const failed = (totals.get("failed") ?? 0) + (totals.get("partial") ?? 0);
  const lag = (d?.kafka?.lags ?? []).reduce((a, l) => a + l.lag, 0);

  const domains = new Map<string, Map<string, number>>();
  for (const r of db?.tasksByDomain ?? []) {
    const m = domains.get(r.key) ?? new Map<string, number>();
    m.set(r.sub ?? "", (m.get(r.sub ?? "") ?? 0) + r.n);
    domains.set(r.key, m);
  }
  const orch = d?.services?.orchestrator;
  const breakers = (d?.services?.adapter ?? []).filter((a) => a.breaker && a.breaker !== "closed");

  return (
    <Page title="Dashboard" sub="Live view of the provisioning platform — refreshes every 5 seconds">
      {d && Object.keys(d.errors).length > 0 && <ErrorBox error={Object.entries(d.errors).map(([k, v]) => `${k}: ${v}`).join(" · ")} />}
      {o.error && <ErrorBox error={o.error} />}
      <Card title="Service health">
        <div className="health">
          {services.map((s) => (
            <Dot key={s} up={d?.up?.[s]} label={s} />
          ))}
        </div>
        {breakers.length > 0 && (
          <p className="small">
            Open circuit breakers:{" "}
            {breakers.map((b) => (
              <Badge key={b.ne} value={`${b.ne} ${b.breaker}`} tone={toneOf(b.breaker)} />
            ))}
          </p>
        )}
      </Card>
      <div className="stats">
        <Stat label="Service orders" value={fmtNum(all)} />
        <Stat label="Completed" value={fmtNum(totals.get("completed") ?? 0)} tone="ok" hint={pct(totals.get("completed") ?? 0, all)} />
        <Stat label="Failed / partial" value={fmtNum(failed)} tone={failed ? "bad" : ""} hint={pct(failed, all)} />
        <Stat label="In flight" value={fmtNum(active)} tone={active ? "info" : ""} />
        <Stat label="Orders / min" value={fmtNum(db?.ordersLastMin ?? 0)} />
        <Stat label="NE tasks / min" value={fmtNum(db?.tasksLastMin ?? 0)} />
        <Stat label="Order p99 (15 min)" value={db?.p99OrderMs ? fmtMs(db.p99OrderMs) : "—"} />
        <Stat label="Outbox backlog" value={fmtNum(db?.outboxBacklog ?? 0)} tone={db?.outboxBacklog ? "warn" : "ok"} />
        <Stat label="Kafka consumer lag" value={fmtNum(lag)} tone={lag > 1000 ? "warn" : "ok"} />
      </div>
      <div className="grid2">
        <Card title="Orders by tenant">
          <Table
            rows={d?.tenants}
            rowKey={(t) => t.tenant}
            columns={[
              { key: "tenant", label: "Tenant", render: (t) => <Link href={`/orders?tenant=${t.tenant}`}>{t.tenant}</Link> },
              { key: "slaTier", label: "SLA", render: (t) => <Badge value={t.slaTier} /> },
              { key: "status", label: "Status", render: (t) => <Badge value={t.status} /> },
              {
                key: "mix",
                label: "State mix",
                render: (t) => {
                  const m = states.get(t.tenant) ?? new Map();
                  return (
                    <StackBar
                      parts={[...m.entries()].map(([k, n]) => ({ label: k, n, tone: toneOf(k) }))}
                    />
                  );
                },
              },
              { key: "orders", label: "Orders", align: "right", render: (t) => fmtNum(t.orders) },
              { key: "failed", label: "Failed", align: "right", render: (t) => fmtNum(t.failed) },
            ]}
          />
          <div className="legend">
            <span><i className="seg ok" />completed</span>
            <span><i className="seg info" />in flight</span>
            <span><i className="seg warn" />held / partial</span>
            <span><i className="seg bad" />failed / rejected</span>
          </div>
        </Card>
        <Card title="NE tasks by domain">
          <Table
            rows={[...domains.entries()].map(([k, m]) => ({ domain: k, m, total: [...m.values()].reduce((a, b) => a + b, 0) }))}
            rowKey={(r) => r.domain}
            columns={[
              { key: "domain", label: "Domain", render: (r) => <Link href={`/tasks?domain=${r.domain}`}>{r.domain}</Link> },
              { key: "mix", label: "Outcome", render: (r) => <StackBar parts={[...r.m.entries()].map(([k, n]) => ({ label: k, n, tone: toneOf(k) }))} /> },
              { key: "total", label: "Tasks", align: "right", render: (r) => fmtNum(r.total) },
              { key: "failed", label: "Failed", align: "right", render: (r) => fmtNum(r.m.get("FAILED") ?? 0) },
            ]}
          />
        </Card>
        <Card title="Network element latency (last 15 minutes)">
          <Table
            rows={db?.neLatency}
            empty="No NE calls in the last 15 minutes — start the load generator to see live traffic"
            rowKey={(r) => r.ne}
            columns={[
              { key: "ne", label: "NE" },
              { key: "calls", label: "Calls", align: "right", render: (r) => fmtNum(r.calls) },
              { key: "p50", label: "p50", align: "right", render: (r) => fmtUs(r.p50us) },
              { key: "p99", label: "p99", align: "right", render: (r) => fmtUs(r.p99us) },
              { key: "errors", label: "Errors", align: "right", render: (r) => (r.errors ? <Badge value={fmtNum(r.errors)} tone="bad" /> : "0") },
            ]}
          />
        </Card>
        <Card title="Orchestrator scheduling">
          {orch ? (
            <dl className="kv">
              <dt>Command lanes</dt>
              <dd>
                {orch.bcLanes.Lanes} lanes · {orch.bcLanes.InFlight} in flight · deepest lane {orch.bcLanes.MaxLaneDepth}
              </dd>
              <dt>Result lanes</dt>
              <dd>
                {orch.resultLanes.Lanes} lanes · {orch.resultLanes.InFlight} in flight · deepest lane {orch.resultLanes.MaxLaneDepth}
              </dd>
              <dt>Queued by priority</dt>
              <dd>{orch.bcScheduler.queued.map((q, i) => `${prio(i)}: ${q}`).join(" · ")}</dd>
              <dt>Served by priority</dt>
              <dd>{orch.bcScheduler.served.map((q, i) => `${prio(i)}: ${fmtNum(q)}`).join(" · ")}</dd>
              <dt>Webhooks</dt>
              <dd>
                {fmtNum(d?.services?.eventhub?.delivered ?? 0)} delivered · {fmtNum(d?.services?.eventhub?.failed ?? 0)} failed
              </dd>
            </dl>
          ) : (
            <p className="muted">Orchestrator statistics unavailable</p>
          )}
        </Card>
      </div>
      <Card title="Most recent orders" actions={<Link href="/orders">All orders →</Link>}>
        <Table
          rows={db?.recent}
          rowKey={(r) => r.tenant + r.id}
          columns={[
            { key: "at", label: "Requested", render: (r) => <span className="nowrap">{fmtTime(r.at)}</span> },
            { key: "tenant", label: "Tenant" },
            { key: "id", label: "Order", render: (r) => <Link className="mono" href={`/orders/${r.tenant}/${r.id}`}>{r.id.slice(0, 8)}</Link> },
            { key: "spec", label: "Service spec" },
            { key: "priority", label: "Prio", render: (r) => prio(r.priority) },
            { key: "state", label: "State", render: (r) => <Badge value={r.state} /> },
            { key: "durationMs", label: "Duration", align: "right", render: (r) => fmtMs(r.durationMs) },
          ]}
        />
      </Card>
    </Page>
  );
}
