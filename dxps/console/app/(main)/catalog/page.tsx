"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useState } from "react";
import { Badge, ErrorBox, Field, Hint, Page, Table } from "@/components/ui";
import { useApi } from "@/lib/hooks";
import type { SpecSummary } from "@/lib/types";

export default function Catalog() {
  const list = useApi<SpecSummary[]>("catalog");
  const router = useRouter();
  const [tenant, setTenant] = useState("");
  const [domain, setDomain] = useState("");
  const [q, setQ] = useState("");
  const rows = (list.data ?? []).filter(
    (s) =>
      (!tenant || s.tenant === tenant) &&
      (!domain || s.domains.includes(domain)) &&
      (!q || `${s.spec} ${s.description}`.toLowerCase().includes(q.toLowerCase())),
  );
  const tenants = [...new Set((list.data ?? []).map((s) => s.tenant))];
  const domains = [...new Set((list.data ?? []).flatMap((s) => s.domains))].sort();

  return (
    <Page
      title="Service catalog"
      sub="Service specifications: parameters, decomposition into NE tasks, and the payload templates they render"
      actions={
        <Link className="button" href="/studio">
          Catalog studio
        </Link>
      }
    >
      <Hint>
        A <b>GLOBAL</b> spec applies to every tenant. A tenant override (same spec code, owned by a tenant) replaces it for that tenant only.
        Changes are made in the Catalog studio as drafts, checked in the Template Lab, then exported into the catalog source.
      </Hint>
      <div className="filters">
        <Field label="Owner">
          <select value={tenant} onChange={(e) => setTenant(e.target.value)}>
            <option value="">All</option>
            {tenants.map((t) => (
              <option key={t}>{t}</option>
            ))}
          </select>
        </Field>
        <Field label="Domain">
          <select value={domain} onChange={(e) => setDomain(e.target.value)}>
            <option value="">All</option>
            {domains.map((t) => (
              <option key={t}>{t}</option>
            ))}
          </select>
        </Field>
        <Field label="Search">
          <input value={q} onChange={(e) => setQ(e.target.value)} placeholder="spec or description" />
        </Field>
      </div>
      <ErrorBox error={list.error} />
      <Table
        rows={list.data ? rows : undefined}
        rowKey={(s) => `${s.tenant}|${s.spec}|${s.version}`}
        onRow={(s) => router.push(`/catalog/${s.tenant}/${s.spec}/${s.version}`)}
        columns={[
          { key: "spec", label: "Spec", render: (s) => <b>{s.spec}</b> },
          { key: "version", label: "Version", render: (s) => <span className="mono">{s.version}</span> },
          { key: "tenant", label: "Owner", render: (s) => <Badge value={s.tenant} tone={s.tenant === "GLOBAL" ? "info" : "warn"} /> },
          { key: "status", label: "Status", render: (s) => <Badge value={s.status} /> },
          { key: "txMode", label: "Mode", render: (s) => <span className="small">{s.txMode}</span> },
          { key: "priority", label: "Prio", render: (s) => s.priority.toUpperCase() },
          { key: "tasks", label: "Tasks", align: "right" },
          { key: "domains", label: "Domains", render: (s) => <div className="chips">{s.domains.map((d) => <span key={d} className="chip">{d}</span>)}</div> },
          { key: "description", label: "Description", render: (s) => <span className="small">{s.description}</span> },
          { key: "file", label: "Source", render: (s) => <span className="mono small">{s.file}</span> },
        ]}
      />
    </Page>
  );
}
