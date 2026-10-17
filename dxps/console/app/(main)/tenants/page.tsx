"use client";

import Link from "next/link";
import { useState } from "react";
import { Can } from "@/components/Shell";
import { Badge, Card, ErrorBox, Field, Hint, Notice, Page, Table } from "@/components/ui";
import { api } from "@/lib/api";
import { fmtNum, fmtTime } from "@/lib/format";
import { useAction, useApi } from "@/lib/hooks";
import type { Tenant } from "@/lib/types";

export default function Tenants() {
  const list = useApi<Tenant[]>("tenants");
  const [sel, setSel] = useState<Tenant>();
  const [form, setForm] = useState({ name: "", slaTier: "SILVER", slaWeight: 1, quotaTps: 1, status: "ACTIVE" });
  const act = useAction();

  const pick = (t: Tenant) => {
    setSel(t);
    setForm({ name: t.name, slaTier: t.slaTier, slaWeight: t.slaWeight, quotaTps: t.quotaTps, status: t.status });
  };
  const save = () =>
    act.run(async () => {
      if (!sel) return;
      await api(`tenants/${sel.tenant}`, { method: "PUT", body: form });
      list.reload();
      return `Tenant ${sel.tenant} updated — the services pick the change up within about 10 seconds`;
    });

  return (
    <Page title="Tenants" sub="Operators and MVNOs sharing the platform: SLA tier, scheduling weight, northbound quota and status">
      <Hint>
        The scheduling weight sets each tenant’s share of NE capacity under load; the quota caps northbound orders per second. Edits are applied
        live and recorded in the audit trail. Note: <code>run-all.sh</code> re-seeds the demo tenants on start.
      </Hint>
      <ErrorBox error={list.error || act.error} />
      <Notice text={act.message} />
      <div className="grid-editor">
        <Card>
          <Table
            rows={list.data}
            rowKey={(t) => t.tenant}
            selected={sel?.tenant}
            onRow={pick}
            columns={[
              { key: "tenant", label: "Tenant", render: (t) => <b>{t.tenant}</b> },
              { key: "name", label: "Name" },
              { key: "tenantType", label: "Type", render: (t) => `${t.tenantType}${t.hostTenant ? ` on ${t.hostTenant}` : ""}` },
              { key: "slaTier", label: "SLA", render: (t) => <Badge value={t.slaTier} /> },
              { key: "slaWeight", label: "Weight", align: "right" },
              { key: "quotaTps", label: "Quota TPS", align: "right", render: (t) => fmtNum(t.quotaTps) },
              { key: "status", label: "Status", render: (t) => <Badge value={t.status} /> },
              { key: "nes", label: "Own / entitled NEs", align: "right", render: (t) => `${t.ownNes} / ${t.entitledNes}` },
              { key: "orders", label: "Orders", align: "right", render: (t) => <Link href={`/orders?tenant=${t.tenant}`}>{fmtNum(t.orders)}</Link> },
              { key: "failed", label: "Failed", align: "right", render: (t) => fmtNum(t.failed) },
              { key: "lastOrder", label: "Last order", render: (t) => <span className="small">{fmtTime(t.lastOrder)}</span> },
            ]}
          />
        </Card>
        <Card title={sel ? `Edit ${sel.tenant}` : "Edit"}>
          {sel ? (
            <Can role="configurator">
              <div className="stack">
                <Field label="Name">
                  <input value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
                </Field>
                <div className="row">
                  <Field label="SLA tier">
                    <select value={form.slaTier} onChange={(e) => setForm({ ...form, slaTier: e.target.value })}>
                      {["GOLD", "SILVER", "BRONZE"].map((x) => (
                        <option key={x}>{x}</option>
                      ))}
                    </select>
                  </Field>
                  <Field label="Weight (1-64)">
                    <input type="number" min={1} max={64} value={form.slaWeight} onChange={(e) => setForm({ ...form, slaWeight: Number(e.target.value) })} />
                  </Field>
                  <Field label="Quota TPS">
                    <input type="number" min={1} max={100000} value={form.quotaTps} onChange={(e) => setForm({ ...form, quotaTps: Number(e.target.value) })} />
                  </Field>
                </div>
                <Field label="Status">
                  <select value={form.status} onChange={(e) => setForm({ ...form, status: e.target.value })}>
                    {["ACTIVE", "SUSPENDED", "OFFBOARDING"].map((x) => (
                      <option key={x}>{x}</option>
                    ))}
                  </select>
                </Field>
                {form.status !== "ACTIVE" && <p className="small muted">A suspended or offboarding tenant’s new orders are rejected by the gateway.</p>}
                <button className="primary" disabled={act.busy} onClick={save}>
                  Save
                </button>
              </div>
            </Can>
          ) : (
            <p className="muted">Select a tenant.</p>
          )}
        </Card>
      </div>
    </Page>
  );
}
