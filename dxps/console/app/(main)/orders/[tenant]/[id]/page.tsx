"use client";

import Link from "next/link";
import { useParams } from "next/navigation";
import { useState } from "react";
import { Can } from "@/components/Shell";
import { Badge, Card, Dag, ErrorBox, Field, Json, Notice, Page, Table, Tabs } from "@/components/ui";
import { api } from "@/lib/api";
import { fmtMs, fmtTime, fmtUs, prio, toneOf } from "@/lib/format";
import { useAction, useApi } from "@/lib/hooks";
import type { OrderDetail, OrderTask } from "@/lib/types";

const views = ["Task graph", "Business commands", "Attempts", "TMF641 request", "Audit"] as const;
const terminal = ["completed", "failed", "cancelled", "rejected", "partial"];

export default function OrderPage() {
  const { tenant, id } = useParams<{ tenant: string; id: string }>();
  const o = useApi<OrderDetail>(`orders/${tenant}/${id}`, 4000);
  const [view, setView] = useState<(typeof views)[number]>("Task graph");
  const [sel, setSel] = useState("");
  const [reason, setReason] = useState("");
  const act = useAction();
  const d = o.data;
  const task: OrderTask | undefined = d?.tasks.find((t) => t.id === sel) ?? d?.tasks[0];

  const cancel = () =>
    act.run(async () => {
      const r = await api<{ status: number; response: unknown }>(`orders/${tenant}/${id}/cancel`, { body: { reason: reason || "cancelled from console" } });
      o.reload();
      return `Gateway answered HTTP ${r.status}`;
    });

  return (
    <Page
      title={`Order ${id.slice(0, 8)}`}
      sub={
        <>
          <Link href="/orders">Service orders</Link> / {tenant} / <span className="mono">{id}</span>
        </>
      }
    >
      <ErrorBox error={o.error || act.error} />
      <Notice text={act.message} />
      {d && (
        <>
          <div className="grid2">
            <Card title="Summary">
              <dl className="kv">
                <dt>State</dt>
                <dd>
                  <Badge value={d.state} /> {d.errorCode && <span className="small">{d.errorCode} {d.error}</span>}
                </dd>
                <dt>Service specs</dt>
                <dd>{d.specs}</dd>
                <dt>Priority / mode</dt>
                <dd>
                  {prio(d.priority)} · {d.txMode}
                </dd>
                <dt>Channel</dt>
                <dd>
                  {d.channel} {d.category && `· ${d.category}`}
                </dd>
                <dt>External id</dt>
                <dd className="mono">{d.externalId || "—"}</dd>
                <dt>Idempotency key</dt>
                <dd className="mono">{d.idempotencyKey}</dd>
                <dt>Requested</dt>
                <dd>{fmtTime(d.requestedAt)}</dd>
                <dt>Completed</dt>
                <dd>
                  {fmtTime(d.completedAt) || "—"} {d.durationMs > 0 && `(${fmtMs(d.durationMs)})`}
                </dd>
              </dl>
            </Card>
            <Card title="Tasks">
              <div className="chips">
                {Object.entries(
                  d.tasks.reduce<Record<string, number>>((m, t) => ((m[t.state] = (m[t.state] ?? 0) + 1), m), {}),
                ).map(([k, n]) => (
                  <Badge key={k} value={`${k} ${n}`} tone={toneOf(k)} />
                ))}
              </div>
              <p className="small muted">
                {d.tasks.length} NE tasks from {d.businessCommands.length} business commands ·{" "}
                {d.tasks.filter((t) => t.compensation).length} compensations · {d.attempts.length} NE attempts
              </p>
              <Can role="operator">
                {!terminal.includes(d.state) && (
                  <div className="row">
                    <Field label="Cancellation reason">
                      <input value={reason} onChange={(e) => setReason(e.target.value)} placeholder="optional" />
                    </Field>
                    <button className="danger" disabled={act.busy} onClick={cancel}>
                      Cancel order
                    </button>
                  </div>
                )}
              </Can>
            </Card>
          </div>
          <Tabs tabs={views} value={view} onChange={setView} />
          {view === "Task graph" && (
            <div className="grid-editor">
              <Card title="Decomposition (click a task)">
                <Dag
                  nodes={d.tasks.map((t) => ({
                    id: t.id,
                    title: `${t.compensation ? "↺ " : ""}${t.specTask || t.operation}`,
                    sub: `${t.ne} · ${t.state}`,
                    tone: toneOf(t.state),
                    deps: t.dependsOn ?? [],
                    dashed: t.compensation,
                  }))}
                  selected={task?.id}
                  onSelect={setSel}
                />
                <p className="small muted">Dashed boxes are compensations (rollback of a completed step). Arrows show dependencies.</p>
              </Card>
              <Card title={task ? `${task.operation} on ${task.ne}` : "Task"}>
                {task && (
                  <>
                    <dl className="kv">
                      <dt>State</dt>
                      <dd>
                        <Badge value={task.state} /> attempt {task.attempt}
                      </dd>
                      <dt>Domain</dt>
                      <dd>{task.domain}</dd>
                      <dt>NE (owner)</dt>
                      <dd>
                        {task.ne} ({task.neTenant})
                      </dd>
                      <dt>NE answer</dt>
                      <dd>
                        {task.neStatus ?? "—"} {task.neCode}
                      </dd>
                      <dt>Coalesce key</dt>
                      <dd className="mono">{task.coalesceKey || "—"}</dd>
                      <dt>After pivot</dt>
                      <dd>{task.afterPivot ? "yes (no rollback)" : "no"}</dd>
                      {task.lastError && (
                        <>
                          <dt>Error</dt>
                          <dd>{task.lastError}</dd>
                        </>
                      )}
                    </dl>
                    <h3>Request sent to the NE</h3>
                    <Json value={task.request} />
                    <h3>NE response</h3>
                    <Json value={task.response} />
                  </>
                )}
              </Card>
            </div>
          )}
          {view === "Business commands" && (
            <Table
              rows={d.businessCommands}
              rowKey={(b) => b.id}
              columns={[
                { key: "itemId", label: "Item" },
                { key: "spec", label: "Spec", render: (b) => <Link href={`/catalog/${b.specTenant}/${b.spec}/${b.specVersion}`}>{`${b.spec} ${b.specVersion}`}</Link> },
                { key: "specTenant", label: "Spec owner" },
                { key: "action", label: "Action" },
                { key: "entityKey", label: "Entity", render: (b) => <span className="mono">{b.entityKey}</span> },
                { key: "state", label: "State", render: (b) => <Badge value={b.state} /> },
                { key: "params", label: "Parameters", render: (b) => <Json value={b.params} /> },
                { key: "error", label: "Error", render: (b) => <span className="small">{b.errorCode} {b.error}</span> },
              ]}
            />
          )}
          {view === "Attempts" && (
            <Table
              rows={d.attempts}
              empty="No NE attempts recorded"
              rowKey={(a) => a.taskId + a.n}
              columns={[
                { key: "startedAt", label: "Started", render: (a) => fmtTime(a.startedAt) },
                { key: "task", label: "Task", render: (a) => d.tasks.find((t) => t.id === a.taskId)?.operation ?? a.taskId.slice(0, 8) },
                { key: "n", label: "#", align: "right" },
                { key: "latencyUs", label: "Latency", align: "right", render: (a) => fmtUs(a.latencyUs) },
                { key: "neStatus", label: "NE status", render: (a) => `${a.neStatus ?? ""} ${a.neCode ?? ""}` },
                { key: "outcome", label: "Outcome", render: (a) => <Badge value={a.outcome} tone={a.outcome === "ok" ? "ok" : "bad"} /> },
                { key: "error", label: "Error", render: (a) => <span className="small">{a.error}</span> },
              ]}
            />
          )}
          {view === "TMF641 request" && (
            <Card>
              <Json value={d.items} />
            </Card>
          )}
          {view === "Audit" && (
            <Table
              rows={d.audit}
              empty="No audit events"
              rowKey={(a) => a.id}
              columns={[
                { key: "at", label: "When", render: (a) => fmtTime(a.at) },
                { key: "actor", label: "Actor" },
                { key: "action", label: "Action" },
                { key: "detail", label: "Detail", render: (a) => <Json value={a.detail} /> },
              ]}
            />
          )}
        </>
      )}
    </Page>
  );
}
