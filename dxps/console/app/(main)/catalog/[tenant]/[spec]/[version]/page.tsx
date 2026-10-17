"use client";

import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useState } from "react";
import { Can } from "@/components/Shell";
import { Badge, Card, Code, Dag, ErrorBox, Field, Page, Table, Tabs } from "@/components/ui";
import { api, qs } from "@/lib/api";
import { useAction, useApi } from "@/lib/hooks";
import type { Draft, SpecDetail, Tenant } from "@/lib/types";

const views = ["Decomposition", "Parameters", "Templates", "YAML source"] as const;

export default function SpecPage() {
  const p = useParams<{ tenant: string; spec: string; version: string }>();
  const router = useRouter();
  const d = useApi<SpecDetail>(`catalog/spec${qs({ tenant: p.tenant, spec: p.spec, version: p.version })}`);
  const tenants = useApi<Tenant[]>("tenants");
  const [view, setView] = useState<(typeof views)[number]>("Decomposition");
  const [task, setTask] = useState("");
  const [forTenant, setForTenant] = useState("");
  const act = useAction();
  const s = d.data?.spec;
  const t = s?.tasks.find((x) => x.id === task) ?? s?.tasks[0];
  const tpl = Object.keys(d.data?.templates ?? {});
  const [shown, setShown] = useState("");
  const shownTpl = shown || tpl[0] || "";

  const override = () =>
    act.run(async () => {
      const dr = await api<Draft>("drafts", {
        body: { name: `${forTenant} override of ${p.spec}`, override: { tenant: p.tenant, spec: p.spec, version: p.version, forTenant } },
      });
      router.push(`/studio/${dr.id}`);
    });
  const edit = () =>
    act.run(async () => {
      if (!d.data) return;
      const dr = await api<Draft>("drafts", { body: { name: `Edit ${p.spec} ${p.version}`, files: { [d.data.file]: d.data.source } } });
      router.push(`/studio/${dr.id}`);
    });

  return (
    <Page
      title={`${p.spec} ${p.version}`}
      sub={
        <>
          <Link href="/catalog">Service catalog</Link> / {p.tenant} / {p.spec}
        </>
      }
      actions={
        <>
          <Can role="configurator">
            <Link className="button" href={`/lab${qs({ tenant: p.tenant === "GLOBAL" ? "" : p.tenant, spec: p.spec, version: p.version })}`}>
              Try in Template Lab
            </Link>
            <button disabled={act.busy || !d.data} onClick={edit}>
              Edit in studio
            </button>
          </Can>
        </>
      }
    >
      <ErrorBox error={d.error || act.error} />
      {s && d.data && (
        <>
          <div className="grid2">
            <Card title="Specification">
              <p>{s.description}</p>
              <dl className="kv">
                <dt>Owner</dt>
                <dd>
                  <Badge value={s.tenant} tone={s.tenant === "GLOBAL" ? "info" : "warn"} />
                </dd>
                <dt>Status</dt>
                <dd>
                  <Badge value={s.status} />
                </dd>
                <dt>Transaction mode</dt>
                <dd>
                  {s.txMode} {s.txMode === "ATOMIC" ? "(all tasks or rollback)" : "(each task stands alone)"}
                </dd>
                <dt>Default priority</dt>
                <dd>
                  {s.priorityDefault} {s.preempt && <Badge value="preempts" tone="warn" />}
                </dd>
                <dt>Entity keys</dt>
                <dd className="mono">{s.entityKeys.join(", ") || "—"}</dd>
                <dt>Versions</dt>
                <dd>{d.data.versions.join(", ")}</dd>
                <dt>Other owners</dt>
                <dd>{d.data.others?.join(", ") || "none"}</dd>
                <dt>Source file</dt>
                <dd className="mono">{d.data.file}</dd>
              </dl>
            </Card>
            <Can role="configurator">
              <Card title="Tenant override">
                <p className="small muted">
                  Create a draft that copies this spec into a tenant-owned override. Edit it in the studio (for example a different task
                  list or template), check it in the Template Lab, then export it.
                </p>
                <div className="row">
                  <Field label="For tenant">
                    <select value={forTenant} onChange={(e) => setForTenant(e.target.value)}>
                      <option value="">Choose…</option>
                      {tenants.data
                        ?.filter((x) => x.tenant !== s.tenant)
                        .map((x) => (
                          <option key={x.tenant}>{x.tenant}</option>
                        ))}
                    </select>
                  </Field>
                  <button className="primary" disabled={!forTenant || act.busy} onClick={override}>
                    Create override draft
                  </button>
                </div>
              </Card>
            </Can>
          </div>
          <Tabs tabs={views} value={view} onChange={setView} />
          {view === "Decomposition" && (
            <div className="grid-editor">
              <Card title="NE task graph">
                <Dag
                  nodes={s.tasks.map((x) => ({
                    id: x.id,
                    title: x.id,
                    sub: `${x.op} · ${x.ne.nfType}`,
                    tone: x.pivot ? "warn" : x.when ? "info" : "ok",
                    deps: x.dependsOn ?? [],
                    dashed: !!x.when,
                  }))}
                  selected={t?.id}
                  onSelect={setTask}
                />
                <p className="small muted">Dashed: conditional task (has a “when” rule). Amber: pivot — tasks after it are not rolled back.</p>
              </Card>
              <Card title={t ? `Task ${t.id}` : "Task"}>
                {t && (
                  <dl className="kv">
                    <dt>Operation</dt>
                    <dd className="mono">{t.op}</dd>
                    <dt>Domain</dt>
                    <dd>{t.domain}</dd>
                    <dt>NE type</dt>
                    <dd className="mono">{t.ne.nfType}</dd>
                    <dt>Depends on</dt>
                    <dd>{t.dependsOn?.join(", ") || "—"}</dd>
                    <dt>When</dt>
                    <dd className="mono">{t.when || "always"}</dd>
                    <dt>Coalesce key</dt>
                    <dd className="mono">{t.coalesceKey || "—"}</dd>
                    <dt>Template</dt>
                    <dd className="mono">
                      {t.template ? (
                        <button className="link" onClick={() => (setShown(t.template!), setView("Templates"))}>
                          {t.template}
                        </button>
                      ) : (
                        "—"
                      )}
                    </dd>
                    <dt>Compensation</dt>
                    <dd className="mono">
                      {t.compensate || "—"} {t.compensateTemplate && `(${t.compensateTemplate})`}
                    </dd>
                    <dt>Pivot</dt>
                    <dd>{t.pivot ? "yes" : "no"}</dd>
                  </dl>
                )}
              </Card>
            </div>
          )}
          {view === "Parameters" && (
            <Card>
              <Table
                rows={Object.entries(s.params.properties).map(([name, v]) => ({ name, ...v }))}
                rowKey={(r) => r.name}
                columns={[
                  { key: "name", label: "Parameter", render: (r) => <b className="mono">{r.name}</b> },
                  { key: "req", label: "Required", render: (r) => (s.params.required?.includes(r.name) ? <Badge value="required" tone="info" /> : "") },
                  { key: "type", label: "Type" },
                  {
                    key: "rule",
                    label: "Rule",
                    render: (r) => (
                      <span className="mono small">
                        {[r.pattern && `pattern ${r.pattern}`, r.enum && `one of ${r.enum.join(", ")}`, r.maxLength && `max ${r.maxLength} chars`,
                          r.minimum !== undefined && `≥ ${r.minimum}`, r.maximum !== undefined && `≤ ${r.maximum}`]
                          .filter(Boolean)
                          .join(" · ")}
                      </span>
                    ),
                  },
                  { key: "example", label: "Example", render: (r) => <span className="mono">{s.example?.[r.name] === undefined ? "" : String(s.example[r.name])}</span> },
                ]}
              />
            </Card>
          )}
          {view === "Templates" && (
            <div className="grid-side">
              <Card title="Templates">
                <ul className="files">
                  {tpl.map((n) => (
                    <li key={n} className={n === shownTpl ? "active" : ""} onClick={() => setShown(n)}>
                      {n}
                    </li>
                  ))}
                </ul>
              </Card>
              <Card title={shownTpl}>
                <Code text={d.data.templates[shownTpl] ?? "(no template)"} />
              </Card>
            </div>
          )}
          {view === "YAML source" && (
            <Card title={d.data.file}>
              <Code text={d.data.source} />
            </Card>
          )}
        </>
      )}
    </Page>
  );
}
