"use client";

import Link from "next/link";
import { useState } from "react";
import { Can } from "@/components/Shell";
import { Badge, Card, ErrorBox, Hint, Json, Notice, Page, Table } from "@/components/ui";
import { api } from "@/lib/api";
import { fmtTime } from "@/lib/format";
import { useAction, useApi } from "@/lib/hooks";
import type { DLQRecord } from "@/lib/types";

export default function DeadLetters() {
  const list = useApi<DLQRecord[]>("dlq?limit=200", 15000);
  const [sel, setSel] = useState<DLQRecord>();
  const act = useAction();
  const replayable = (list.data ?? []).filter((r) => r.sourceTopic).length;

  const redrive = () =>
    act.run(async () => {
      if (!window.confirm("Replay every dead letter that carries a source topic back onto that topic?")) return;
      const r = await api<{ replayed: number; skipped: number }>("dlq/redrive", { body: {} });
      list.reload();
      return `Replayed ${r.replayed} record(s), skipped ${r.skipped}`;
    });

  const order = (r: DLQRecord) => {
    const p = r.payload ?? {};
    const tenant = r.headers.tenant ?? String(p.tenant ?? "");
    const id = String(p.order ?? p.orderId ?? "");
    return tenant && id ? { tenant, id } : undefined;
  };

  return (
    <Page
      title="Dead letters"
      sub="Latest records on the dxps.dlq topic (read without joining a consumer group)"
      actions={
        <Can role="operator">
          <button className="primary" disabled={act.busy} onClick={redrive}>
            Redrive ({replayable})
          </button>
        </Can>
      }
    >
      <Hint>
        Records produced by a consumer that could not process a message carry a <code>source_topic</code> header and can be redriven. Records
        produced by the orchestrator itself are final NE failure notices (for example <code>DXPS-3404</code>): they are kept for analysis and
        skipped by redrive — fix the cause and resubmit the order instead.
      </Hint>
      <ErrorBox error={list.error || act.error} />
      <Notice text={act.message} />
      <div className="grid-editor">
        <Card>
          <Table
            rows={list.data}
            empty="The dead-letter topic is empty"
            rowKey={(r) => `${r.partition}/${r.offset}`}
            selected={sel ? `${sel.partition}/${sel.offset}` : ""}
            onRow={setSel}
            columns={[
              { key: "at", label: "When", render: (r) => <span className="nowrap">{fmtTime(r.at)}</span> },
              { key: "po", label: "Partition / offset", render: (r) => `${r.partition} / ${r.offset}` },
              { key: "tenant", label: "Tenant", render: (r) => r.headers.tenant ?? "" },
              { key: "producer", label: "Producer", render: (r) => r.headers.producer_app ?? "" },
              { key: "code", label: "Code", render: (r) => <span className="mono">{String(r.payload?.code ?? r.error ?? "")}</span> },
              { key: "op", label: "Operation", render: (r) => <span className="mono">{String(r.payload?.operation ?? "")}</span> },
              {
                key: "source",
                label: "Redrive",
                render: (r) => (r.sourceTopic ? <Badge value={r.sourceTopic} tone="info" /> : <Badge value="final" tone="muted" />),
              },
            ]}
          />
        </Card>
        <Card title="Record">
          {sel ? (
            <>
              {order(sel) && (
                <p>
                  <Link href={`/orders/${order(sel)!.tenant}/${order(sel)!.id}`}>Open order {order(sel)!.id.slice(0, 8)} →</Link>
                </p>
              )}
              <dl className="kv">
                <dt>Key</dt>
                <dd className="mono">{sel.key}</dd>
                <dt>Message</dt>
                <dd>{String(sel.payload?.message ?? sel.error ?? "")}</dd>
              </dl>
              <h3>Headers</h3>
              <Json value={sel.headers} />
              <h3>Payload</h3>
              <Json value={sel.payload ?? sel.text} />
            </>
          ) : (
            <p className="muted">Select a record to inspect it.</p>
          )}
        </Card>
      </div>
    </Page>
  );
}
