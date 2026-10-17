"use client";

import Link from "next/link";
import { useState } from "react";
import { ErrorBox, Field, Json, Page, Table, Tabs } from "@/components/ui";
import { qs } from "@/lib/api";
import { fmtTime } from "@/lib/format";
import { useApi } from "@/lib/hooks";
import type { AuditRow, ConsoleAudit, Tenant } from "@/lib/types";

const sources = ["Platform", "Console"] as const;

export default function Audit() {
  const [src, setSrc] = useState<(typeof sources)[number]>("Platform");
  const [tenant, setTenant] = useState("");
  const [action, setAction] = useState("");
  const [q, setQ] = useState("");
  const [query, setQuery] = useState("");
  const tenants = useApi<Tenant[]>("tenants");
  const platform = useApi<AuditRow[]>(src === "Platform" ? `audit${qs({ tenant, action, q: query, limit: 300 })}` : null);
  const console_ = useApi<ConsoleAudit[]>(src === "Console" ? `audit${qs({ source: "console", q: query, limit: 300 })}` : null);

  return (
    <Page title="Audit trail" sub="Platform audit events recorded per tenant in the database, and actions taken in this console">
      <Tabs tabs={sources} value={src} onChange={setSrc} />
      <form className="filters" onSubmit={(e) => (e.preventDefault(), setQuery(q))}>
        {src === "Platform" && (
          <>
            <Field label="Tenant">
              <select value={tenant} onChange={(e) => setTenant(e.target.value)}>
                <option value="">All tenants</option>
                {tenants.data?.map((t) => (
                  <option key={t.tenant}>{t.tenant}</option>
                ))}
              </select>
            </Field>
            <Field label="Action">
              <input value={action} onChange={(e) => setAction(e.target.value)} placeholder="e.g. order.created" />
            </Field>
          </>
        )}
        <Field label="Search">
          <input value={q} onChange={(e) => setQ(e.target.value)} placeholder="actor, object, detail" />
        </Field>
        <button className="primary">Search</button>
      </form>
      <ErrorBox error={platform.error || console_.error} />
      {src === "Platform" ? (
        <Table
          rows={platform.data}
          empty="No audit events"
          rowKey={(a) => a.tenant + a.id}
          columns={[
            { key: "at", label: "When", render: (a) => <span className="nowrap">{fmtTime(a.at)}</span> },
            { key: "tenant", label: "Tenant" },
            { key: "actor", label: "Actor", render: (a) => <span className="mono small">{a.actor}{a.actingForTenant ? " (for tenant)" : ""}</span> },
            { key: "action", label: "Action", render: (a) => <span className="mono">{a.action}</span> },
            {
              key: "object",
              label: "Object",
              render: (a) =>
                a.objectType === "service_order" && a.objectId ? (
                  <Link className="mono small" href={`/orders/${a.tenant}/${a.objectId}`}>
                    order {a.objectId.slice(0, 8)}
                  </Link>
                ) : (
                  <span className="mono small">
                    {a.objectType} {a.objectId}
                  </span>
                ),
            },
            { key: "detail", label: "Detail", render: (a) => (a.detail ? <details><summary>show</summary><Json value={a.detail} /></details> : "") },
          ]}
        />
      ) : (
        <Table
          rows={console_.data}
          empty="No console actions recorded"
          rowKey={(a, i) => a.at + i}
          columns={[
            { key: "at", label: "When", render: (a) => <span className="nowrap">{fmtTime(a.at)}</span> },
            { key: "actor", label: "User" },
            { key: "action", label: "Action", render: (a) => <span className="mono">{a.action}</span> },
            { key: "object", label: "Object", render: (a) => <span className="mono small">{a.object}</span> },
            { key: "detail", label: "Detail", render: (a) => (a.detail ? <details><summary>show</summary><Json value={a.detail} /></details> : "") },
          ]}
        />
      )}
    </Page>
  );
}
