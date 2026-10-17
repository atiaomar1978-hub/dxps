"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useState } from "react";
import { Can } from "@/components/Shell";
import { Badge, ErrorBox, Field, Page, Pager, Table } from "@/components/ui";
import { qs } from "@/lib/api";
import { fmtMs, fmtTime, orderStates, prio } from "@/lib/format";
import { useApi } from "@/lib/hooks";
import type { OrderRow, Tenant } from "@/lib/types";

const limit = 50;

export default function Orders() {
  const sp = useSearchParams();
  const router = useRouter();
  const [tenant, setTenant] = useState(sp.get("tenant") ?? "");
  const [state, setState] = useState(sp.get("state") ?? "");
  const [spec, setSpec] = useState(sp.get("spec") ?? "");
  const [q, setQ] = useState("");
  const [query, setQuery] = useState("");
  const [offset, setOffset] = useState(0);
  const tenants = useApi<Tenant[]>("tenants");
  const list = useApi<{ rows: OrderRow[]; total: number }>(`orders${qs({ tenant, state, spec, q: query, limit, offset })}`, 10000);

  return (
    <Page
      title="Service orders"
      sub="TMF641 service orders across all tenants (read through the platform operations role)"
      actions={
        <Can role="operator">
          <Link className="button" href="/compose">
            New order
          </Link>
        </Can>
      }
    >
      <form
        className="filters"
        onSubmit={(e) => {
          e.preventDefault();
          setOffset(0);
          setQuery(q);
        }}
      >
        <Field label="Tenant">
          <select value={tenant} onChange={(e) => (setTenant(e.target.value), setOffset(0))}>
            <option value="">All tenants</option>
            {tenants.data?.map((t) => (
              <option key={t.tenant}>{t.tenant}</option>
            ))}
          </select>
        </Field>
        <Field label="State">
          <select value={state} onChange={(e) => (setState(e.target.value), setOffset(0))}>
            <option value="">Any state</option>
            {orderStates.map((s) => (
              <option key={s}>{s}</option>
            ))}
          </select>
        </Field>
        <Field label="Service spec">
          <input value={spec} onChange={(e) => (setSpec(e.target.value), setOffset(0))} placeholder="e.g. ActivateSubscriber" />
        </Field>
        <Field label="Search">
          <input value={q} onChange={(e) => setQ(e.target.value)} placeholder="order id, external id, error" />
        </Field>
        <button className="primary">Search</button>
      </form>
      <ErrorBox error={list.error} />
      <Table
        rows={list.data?.rows}
        rowKey={(r) => r.tenant + r.id}
        onRow={(r) => router.push(`/orders/${r.tenant}/${r.id}`)}
        columns={[
          { key: "requestedAt", label: "Requested", render: (r) => <span className="nowrap">{fmtTime(r.requestedAt)}</span> },
          { key: "tenant", label: "Tenant" },
          { key: "id", label: "Order", render: (r) => <span className="mono">{r.id.slice(0, 8)}</span> },
          { key: "externalId", label: "External id", render: (r) => <span className="mono clip">{r.externalId}</span> },
          { key: "specs", label: "Service specs", render: (r) => <span className="clip">{r.specs}</span> },
          { key: "priority", label: "Prio", render: (r) => prio(r.priority) },
          { key: "txMode", label: "Mode", render: (r) => <span className="small">{r.txMode}</span> },
          { key: "tasks", label: "Tasks", align: "right" },
          { key: "state", label: "State", render: (r) => <Badge value={r.state} /> },
          { key: "durationMs", label: "Duration", align: "right", render: (r) => fmtMs(r.durationMs) },
          { key: "error", label: "Error", render: (r) => <span className="small clip" title={r.error}>{r.errorCode} {r.error}</span> },
        ]}
      />
      <Pager offset={offset} limit={limit} total={list.data?.total ?? 0} onChange={setOffset} />
    </Page>
  );
}
