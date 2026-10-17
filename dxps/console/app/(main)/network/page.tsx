"use client";

import Link from "next/link";
import { useState } from "react";
import { Can } from "@/components/Shell";
import { Badge, Card, ErrorBox, Field, Json, Notice, Page, Table } from "@/components/ui";
import { api } from "@/lib/api";
import { fmtNum, fmtTime, fmtUs, pct } from "@/lib/format";
import { useAction, useApi } from "@/lib/hooks";
import type { Exchange, NE, SimStat } from "@/lib/types";

export default function Network() {
  const nes = useApi<NE[]>("nes", 10000);
  const sims = useApi<{ sims: SimStat[]; exchanges: Exchange[] | null }>("sims", 5000);
  const [sim, setSim] = useState("");
  const [latency, setLatency] = useState(0);
  const [rate, setRate] = useState(0);
  const [ex, setEx] = useState<Exchange>();
  const act = useAction();

  const pick = (s: SimStat) => {
    setSim(s.name);
    setLatency(s.latencyMs);
    setRate(s.errorRate);
  };
  const apply = (latencyMs: number, errorRate: number) =>
    act.run(async () => {
      await api("sims/fault", { body: { sim, latencyMs, errorRate } });
      sims.reload();
      return `${sim}: latency ${latencyMs} ms, error rate ${(errorRate * 100).toFixed(0)}%`;
    });

  return (
    <Page title="Network & simulators" sub="Network elements from the inventory with live adapter health, and the NE simulators behind them">
      <ErrorBox error={nes.error || sims.error || act.error} />
      <Notice text={act.message} />
      <Card title="Network elements">
        <Table
          rows={nes.data}
          rowKey={(n) => n.tenant + n.code}
          columns={[
            { key: "code", label: "NE", render: (n) => <Link href={`/tasks?ne=${n.code}`}>{n.code}</Link> },
            { key: "tenant", label: "Owner" },
            { key: "domain", label: "Domain" },
            { key: "nfType", label: "NF type", render: (n) => <span className="mono">{n.nfType}</span> },
            { key: "vendor", label: "Vendor" },
            { key: "endpoint", label: "Endpoint", render: (n) => <span className="mono small">{n.endpoints.map((e) => `${e.adapterType} ${e.baseUri}`).join(", ")}</span> },
            { key: "shared", label: "Shared", render: (n) => (n.shared ? <Badge value={`${Object.keys(n.access ?? {}).length} tenants`} tone="info" /> : "") },
            { key: "limits", label: "Max TPS / conc.", align: "right", render: (n) => `${fmtNum(n.maxTps)} / ${n.maxConcurrency}` },
            { key: "calls", label: "Calls 15m", align: "right", render: (n) => fmtNum(n.calls15m) },
            { key: "p99", label: "p99", align: "right", render: (n) => fmtUs(n.p99us) },
            { key: "errors", label: "Errors", align: "right", render: (n) => (n.errors15m ? <Badge value={pct(n.errors15m, n.calls15m)} tone="bad" /> : "0") },
            { key: "breaker", label: "Breaker", render: (n) => (n.breaker ? <Badge value={n.breaker} /> : <span className="muted">—</span>) },
          ]}
        />
      </Card>
      <div className="grid-editor">
        <Card title="Simulators">
          <Table
            rows={sims.data?.sims}
            rowKey={(s) => s.name}
            selected={sim}
            onRow={pick}
            columns={[
              { key: "name", label: "Simulator" },
              { key: "port", label: "Port", align: "right" },
              { key: "calls", label: "Calls", align: "right", render: (s) => fmtNum(s.calls) },
              { key: "byStatus", label: "Status codes", render: (s) => <span className="small">{Object.entries(s.byStatus).map(([k, v]) => `${k}: ${v}`).join(" · ")}</span> },
              { key: "p99", label: "p99", align: "right", render: (s) => fmtUs(s.p99us) },
              { key: "objects", label: "Objects", align: "right", render: (s) => fmtNum(s.objects) },
              {
                key: "fault",
                label: "Fault profile",
                render: (s) => (
                  <Badge value={`${s.latencyMs} ms · ${(s.errorRate * 100).toFixed(0)}% errors`} tone={s.errorRate > 0 || s.latencyMs > 100 ? "warn" : "ok"} />
                ),
              },
            ]}
          />
        </Card>
        <Card title="Fault injection">
          <Can role="operator">
            {sim ? (
              <div className="stack">
                <p className="muted small">
                  Changes how <b>{sim}</b> answers: added latency per request and the share of requests answered with an error. Use it to
                  watch retries, circuit breakers and compensations.
                </p>
                <Field label={`Latency: ${latency} ms`}>
                  <input type="range" min={0} max={2000} step={10} value={latency} onChange={(e) => setLatency(Number(e.target.value))} />
                </Field>
                <Field label={`Error rate: ${(rate * 100).toFixed(0)}%`}>
                  <input type="range" min={0} max={1} step={0.05} value={rate} onChange={(e) => setRate(Number(e.target.value))} />
                </Field>
                <div className="actions">
                  <button className="primary" disabled={act.busy} onClick={() => apply(latency, rate)}>
                    Apply
                  </button>
                  <button disabled={act.busy} onClick={() => (setLatency(1), setRate(0), apply(1, 0))}>
                    Reset to healthy
                  </button>
                </div>
              </div>
            ) : (
              <p className="muted">Select a simulator on the left.</p>
            )}
          </Can>
        </Card>
      </div>
      <div className="grid-editor">
        <Card title="Recent NE exchanges (latest 40)">
          <Table
            rows={(sims.data?.exchanges ?? []).slice(0, 40)}
            empty="No exchanges captured since the simulators started"
            rowKey={(e, i) => e.at + i}
            onRow={setEx}
            columns={[
              { key: "at", label: "When", render: (e) => <span className="nowrap">{fmtTime(e.at)}</span> },
              { key: "sim", label: "Simulator" },
              { key: "tenant", label: "Tenant" },
              { key: "req", label: "Request", render: (e) => <span className="mono small clip" title={e.path}>{e.method} {e.path}</span> },
              { key: "status", label: "Status", render: (e) => <Badge value={String(e.status)} tone={e.status < 300 ? "ok" : e.status < 500 ? "warn" : "bad"} /> },
              { key: "lat", label: "Latency", align: "right", render: (e) => fmtUs(e.latUs) },
              { key: "fault", label: "Fault", render: (e) => e.fault ?? "" },
            ]}
          />
        </Card>
        <Card title="Exchange">
          {ex ? (
            <>
              <h3>Request</h3>
              <Json value={ex.request} />
              <h3>Response</h3>
              <Json value={ex.response} />
            </>
          ) : (
            <p className="muted">Select an exchange to see the payloads.</p>
          )}
        </Card>
      </div>
    </Page>
  );
}
