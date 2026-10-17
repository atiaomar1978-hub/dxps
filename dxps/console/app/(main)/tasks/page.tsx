"use client";

import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { useState } from "react";
import { Badge, ErrorBox, Field, Page, Table } from "@/components/ui";
import { qs } from "@/lib/api";
import { domains, fmtTime, prio, taskStates } from "@/lib/format";
import { useApi } from "@/lib/hooks";
import type { NE, TaskRow, Tenant } from "@/lib/types";

export default function Tasks() {
  const sp = useSearchParams();
  const [tenant, setTenant] = useState(sp.get("tenant") ?? "");
  const [state, setState] = useState(sp.get("state") ?? "");
  const [domain, setDomain] = useState(sp.get("domain") ?? "");
  const [ne, setNe] = useState(sp.get("ne") ?? "");
  const tenants = useApi<Tenant[]>("tenants");
  const nes = useApi<NE[]>("nes");
  const list = useApi<TaskRow[]>(`tasks${qs({ tenant, state, domain, ne, limit: 200 })}`, 10000);

  return (
    <Page title="NE tasks" sub="Southbound commands executed against network elements (latest 200 matching)">
      <div className="filters">
        <Field label="Tenant">
          <select value={tenant} onChange={(e) => setTenant(e.target.value)}>
            <option value="">All tenants</option>
            {tenants.data?.map((t) => (
              <option key={t.tenant}>{t.tenant}</option>
            ))}
          </select>
        </Field>
        <Field label="State">
          <select value={state} onChange={(e) => setState(e.target.value)}>
            <option value="">Any state</option>
            {taskStates.map((s) => (
              <option key={s}>{s}</option>
            ))}
          </select>
        </Field>
        <Field label="Domain">
          <select value={domain} onChange={(e) => setDomain(e.target.value)}>
            <option value="">Any domain</option>
            {domains.map((s) => (
              <option key={s}>{s}</option>
            ))}
          </select>
        </Field>
        <Field label="Network element">
          <select value={ne} onChange={(e) => setNe(e.target.value)}>
            <option value="">Any NE</option>
            {nes.data?.map((n) => (
              <option key={n.code}>{n.code}</option>
            ))}
          </select>
        </Field>
      </div>
      <ErrorBox error={list.error} />
      <Table
        rows={list.data}
        rowKey={(r) => r.id}
        columns={[
          { key: "updatedAt", label: "Updated", render: (r) => <span className="nowrap">{fmtTime(r.updatedAt)}</span> },
          { key: "tenant", label: "Tenant" },
          { key: "order", label: "Order", render: (r) => <Link className="mono" href={`/orders/${r.tenant}/${r.orderId}`}>{r.orderId.slice(0, 8)}</Link> },
          { key: "ne", label: "NE" },
          { key: "domain", label: "Domain" },
          { key: "operation", label: "Operation", render: (r) => <span className="mono">{r.compensation ? "↺ " : ""}{r.operation}</span> },
          { key: "priority", label: "Prio", render: (r) => prio(r.priority) },
          { key: "attempt", label: "Attempt", align: "right" },
          { key: "neStatus", label: "NE answer", render: (r) => `${r.neStatus ?? ""} ${r.neCode ?? ""}` },
          { key: "state", label: "State", render: (r) => <Badge value={r.state} /> },
          { key: "lastError", label: "Error", render: (r) => <span className="small clip" title={r.lastError}>{r.lastError}</span> },
        ]}
      />
    </Page>
  );
}
