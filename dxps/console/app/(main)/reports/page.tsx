"use client";

import { useState } from "react";
import { Badge, Card, ErrorBox, Field, Page, StackBar, Stat, Table } from "@/components/ui";
import { fmtMs, fmtNum, fmtUs, pct, toneOf } from "@/lib/format";
import { useApi } from "@/lib/hooks";
import type { Report } from "@/lib/types";

export default function Reports() {
  const [days, setDays] = useState(30);
  const r = useApi<Report>(`reports?days=${days}`);
  const d = r.data;

  const byDay = new Map<string, Map<string, number>>();
  for (const x of d?.byDay ?? []) {
    const m = byDay.get(x.day) ?? new Map<string, number>();
    m.set(x.state, (m.get(x.state) ?? 0) + x.n);
    byDay.set(x.day, m);
  }
  const dayRows = [...byDay.entries()].sort().map(([day, m]) => ({ day, m, total: [...m.values()].reduce((a, b) => a + b, 0) }));
  const maxDay = Math.max(1, ...dayRows.map((x) => x.total));
  const total = dayRows.reduce((a, x) => a + x.total, 0);
  const completed = dayRows.reduce((a, x) => a + (x.m.get("completed") ?? 0), 0);
  const failed = dayRows.reduce((a, x) => a + (x.m.get("failed") ?? 0) + (x.m.get("partial") ?? 0), 0);

  return (
    <Page
      title="Reports"
      sub="Order volume, success rate, latency by service and NE, and the most frequent errors"
      actions={
        <Field label="Period">
          <select value={days} onChange={(e) => setDays(Number(e.target.value))}>
            {[1, 7, 30, 90].map((n) => (
              <option key={n} value={n}>
                last {n} day{n > 1 ? "s" : ""}
              </option>
            ))}
          </select>
        </Field>
      }
    >
      <ErrorBox error={r.error} />
      <div className="stats">
        <Stat label="Orders" value={fmtNum(total)} />
        <Stat label="Success rate" value={pct(completed, total) || "—"} tone="ok" />
        <Stat label="Failed / partial" value={fmtNum(failed)} tone={failed ? "bad" : ""} />
        <Stat label="Service specs used" value={d?.bySpec.length ?? 0} />
        <Stat label="NEs called" value={d?.byNe.length ?? 0} />
      </div>
      <div className="grid2">
        <Card title="Orders per day">
          <Table
            rows={d ? dayRows : undefined}
            empty="No orders in this period"
            rowKey={(x) => x.day}
            columns={[
              { key: "day", label: "Day", render: (x) => <span className="mono">{x.day}</span> },
              {
                key: "mix",
                label: "",
                render: (x) => (
                  <div style={{ width: `${(100 * x.total) / maxDay}%` }}>
                    <StackBar parts={[...x.m.entries()].map(([k, n]) => ({ label: k, n, tone: toneOf(k) }))} />
                  </div>
                ),
              },
              { key: "total", label: "Orders", align: "right", render: (x) => fmtNum(x.total) },
              { key: "ok", label: "Success", align: "right", render: (x) => pct(x.m.get("completed") ?? 0, x.total) },
            ]}
          />
        </Card>
        <Card title="By service spec">
          <Table
            rows={d?.bySpec}
            empty="No order items in this period"
            rowKey={(x) => x.spec}
            columns={[
              { key: "spec", label: "Spec" },
              { key: "items", label: "Items", align: "right", render: (x) => fmtNum(x.items) },
              { key: "ok", label: "Success", align: "right", render: (x) => pct(x.completed, x.items) },
              { key: "failed", label: "Failed", align: "right", render: (x) => fmtNum(x.failed) },
              { key: "p50", label: "p50", align: "right", render: (x) => fmtMs(x.p50ms) },
              { key: "p95", label: "p95", align: "right", render: (x) => fmtMs(x.p95ms) },
            ]}
          />
        </Card>
        <Card title="By network element">
          <Table
            rows={d?.byNe}
            empty="No NE attempts in this period"
            rowKey={(x) => x.ne}
            columns={[
              { key: "ne", label: "NE" },
              { key: "calls", label: "Attempts", align: "right", render: (x) => fmtNum(x.calls) },
              { key: "errors", label: "Errors", align: "right", render: (x) => `${fmtNum(x.errors)} (${pct(x.errors, x.calls)})` },
              { key: "p50", label: "p50", align: "right", render: (x) => fmtUs(x.p50us) },
              { key: "p99", label: "p99", align: "right", render: (x) => fmtUs(x.p99us) },
            ]}
          />
        </Card>
        <Card title="Top errors">
          <Table
            rows={d?.errors}
            empty="No errors in this period"
            rowKey={(x) => x.code + x.tenant}
            columns={[
              { key: "code", label: "Code", render: (x) => <Badge value={x.code} tone="bad" /> },
              { key: "tenant", label: "Tenant" },
              { key: "n", label: "Count", align: "right", render: (x) => fmtNum(x.n) },
              { key: "sample", label: "Example", render: (x) => <span className="small">{x.sample}</span> },
            ]}
          />
        </Card>
      </div>
    </Page>
  );
}
