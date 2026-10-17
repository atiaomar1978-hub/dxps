"use client";

import { useState } from "react";
import { Can } from "@/components/Shell";
import { Badge, Card, ErrorBox, Field, Hint, Notice, Page, Spark, Stat, Table } from "@/components/ui";
import { api } from "@/lib/api";
import { fmtMs, fmtNum, fmtTime } from "@/lib/format";
import { useAction, useApi } from "@/lib/hooks";
import type { LoadGen } from "@/lib/types";

export default function LoadPage() {
  const g = useApi<LoadGen>("loadgen", 3000);
  const [count, setCount] = useState(500);
  const [rate, setRate] = useState(50);
  const [chaos, setChaos] = useState(0.05);
  const act = useAction();
  const demo = g.data?.demo;
  const hist = g.data?.history ?? [];
  const probes = g.data?.probes ?? [];
  const passed = probes.filter((p) => p.pass).length;

  const start = () =>
    act.run(async () => {
      await api("loadgen", { body: { count, rate, chaos } });
      g.reload();
      return `Started ${count} orders at ${rate}/s`;
    });
  const probe = () =>
    act.run(async () => {
      await api("probes", { body: {} });
      g.reload();
      return "Security probes started";
    });

  return (
    <Page title="Load & probes" sub="Drive realistic order traffic across tenants and run the negative security probes against the gateway">
      <Hint>
        The generator sends a mix of service orders for all active tenants through the real gateway. <b>Chaos</b> is the share of orders built
        to fail (invalid parameters, unknown subscribers) so you can watch rejections, NE errors, compensations and dead letters.
      </Hint>
      <ErrorBox error={g.error || act.error} />
      <Notice text={act.message} />
      <div className="grid-editor">
        <Card title="Load generator" actions={demo && <Badge value={demo.running ? "running" : "idle"} tone={demo.running ? "info" : "muted"} />}>
          <div className="stats">
            <Stat label="Sent" value={`${fmtNum(demo?.sent ?? 0)} / ${fmtNum(demo?.target ?? 0)}`} />
            <Stat label="Gateway p50" value={fmtMs(demo?.p50ms ?? 0)} />
            <Stat label="Gateway p99" value={fmtMs(demo?.p99ms ?? 0)} />
            <Stat label="Started" value={<span className="small">{fmtTime(demo?.started) || "—"}</span>} />
          </div>
          <div className="grid3">
            <div>
              <h3>HTTP status</h3>
              <div className="chips">
                {Object.entries(demo?.byStatus ?? {}).map(([k, v]) => (
                  <Badge key={k} value={`${k}: ${v}`} tone={k.startsWith("2") ? "ok" : k.startsWith("4") ? "warn" : "bad"} />
                ))}
              </div>
            </div>
            <div>
              <h3>Error codes</h3>
              <div className="chips">
                {Object.entries(demo?.byCode ?? {}).map(([k, v]) => (
                  <Badge key={k} value={`${k}: ${v}`} tone="bad" />
                ))}
              </div>
            </div>
            <div>
              <h3>Specs</h3>
              <div className="chips">
                {Object.entries(demo?.bySpec ?? {}).map(([k, v]) => (
                  <span key={k} className="chip">
                    {k}: {v}
                  </span>
                ))}
              </div>
            </div>
          </div>
          {demo?.lastError && <p className="small">Last error: {demo.lastError}</p>}
          <h3>Platform throughput (one sample per minute)</h3>
          <div className="sparks">
            <Spark label="orders/min" values={hist.map((h) => h.orders)} />
            <Spark label="tasks/min" values={hist.map((h) => h.tasks)} />
            <Spark label="p99 ms" values={hist.map((h) => Math.round(h.p99))} />
            <Spark label="Kafka lag" values={hist.map((h) => h.lag)} />
          </div>
        </Card>
        <Card title="Start a run">
          <Can role="operator">
            <div className="stack">
              <Field label="Orders (1-20000)">
                <input type="number" min={1} max={20000} value={count} onChange={(e) => setCount(Number(e.target.value))} />
              </Field>
              <Field label="Rate per second (1-500)">
                <input type="number" min={1} max={500} value={rate} onChange={(e) => setRate(Number(e.target.value))} />
              </Field>
              <Field label={`Chaos: ${(chaos * 100).toFixed(0)}% of orders built to fail`}>
                <input type="range" min={0} max={1} step={0.01} value={chaos} onChange={(e) => setChaos(Number(e.target.value))} />
              </Field>
              <button className="primary" disabled={act.busy || demo?.running} onClick={start}>
                Start
              </button>
            </div>
          </Can>
        </Card>
      </div>
      <Card
        title="Security probes"
        actions={
          <Can role="operator">
            <button disabled={act.busy || demo?.probing} onClick={probe}>
              {demo?.probing ? "Probing…" : "Run probes"}
            </button>
          </Can>
        }
      >
        {probes.length > 0 && (
          <p>
            <Badge value={`${passed} / ${probes.length} passed`} tone={passed === probes.length ? "ok" : "bad"} />
          </p>
        )}
        <Table
          rows={probes}
          empty="No probe run yet"
          rowKey={(p) => p.id}
          columns={[
            { key: "category", label: "Category" },
            { key: "name", label: "Probe" },
            { key: "expect", label: "Expected", render: (p) => <span className="small">{p.expect}</span> },
            { key: "got", label: "Got", render: (p) => <span className="small">{p.got}</span> },
            { key: "pass", label: "Result", render: (p) => <Badge value={p.pass ? "pass" : "fail"} /> },
          ]}
        />
      </Card>
    </Page>
  );
}
