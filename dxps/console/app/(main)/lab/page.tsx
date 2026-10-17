"use client";

import { useRouter, useSearchParams } from "next/navigation";
import { useEffect, useState } from "react";
import { Badge, Card, Dag, ErrorBox, Field, Hint, Json, Page, Tabs } from "@/components/ui";
import { api, qs } from "@/lib/api";
import { composeKey, parseJson } from "@/lib/format";
import { useAction, useApi } from "@/lib/hooks";
import type { DraftSummary, LabResult, LabTask, SpecDetail, SpecSummary, Tenant } from "@/lib/types";

const views = ["Payload", "Compensation", "Selection"] as const;

function tone(t: LabTask): string {
  if (!t.applies) return "muted";
  if (t.neError || t.renderError || t.whenError) return "bad";
  return t.pivot ? "warn" : "ok";
}

export default function Lab() {
  const sp = useSearchParams();
  const router = useRouter();
  const tenants = useApi<Tenant[]>("tenants");
  const catalog = useApi<SpecSummary[]>("catalog");
  const drafts = useApi<DraftSummary[]>("drafts");
  const [tenant, setTenant] = useState(sp.get("tenant") || "host-mno");
  const [spec, setSpec] = useState(sp.get("spec") ?? "");
  const [version, setVersion] = useState(sp.get("version") ?? "");
  const [draft, setDraft] = useState(sp.get("draft") ?? "");
  const [params, setParams] = useState("{}");
  const [results, setResults] = useState("");
  const [res, setRes] = useState<LabResult>();
  const [sel, setSel] = useState("");
  const [view, setView] = useState<(typeof views)[number]>("Payload");
  const act = useAction();

  const codes = [...new Set((catalog.data ?? []).map((s) => s.spec))].sort();
  const owner = (catalog.data ?? []).find((s) => s.spec === spec && s.tenant === tenant) ? tenant : "GLOBAL";
  const versions = (catalog.data ?? []).filter((s) => s.spec === spec && s.tenant === owner).map((s) => s.version);

  useEffect(() => {
    if (!spec && codes.length) setSpec(codes[0]);
  }, [codes, spec]);

  // Prefill the parameters with the spec's example whenever the resolved spec changes.
  useEffect(() => {
    if (!spec || !catalog.data) return;
    const v = version || versions[versions.length - 1] || "";
    if (!v) return;
    let live = true;
    api<SpecDetail>(`catalog/spec${qs({ tenant: owner, spec, version: v })}`)
      .then((d) => live && setParams(JSON.stringify(d.spec.example ?? {}, null, 2)))
      .catch(() => undefined);
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [spec, owner, version, catalog.data]);

  const p = parseJson<Record<string, unknown>>(params);
  const r = results.trim() ? parseJson<Record<string, Record<string, unknown>>>(results) : { value: undefined };

  const run = () =>
    act.run(async () => {
      const out = await api<LabResult>("lab", { body: { tenant, spec, version, draft, params: p.value, results: r.value } });
      setRes(out);
      setSel(out.tasks[0]?.id ?? "");
    });

  const send = () => {
    sessionStorage.setItem(composeKey, JSON.stringify({ tenant, items: [{ id: "1", spec, action: "add", params: p.value ?? {} }] }));
    router.push("/compose");
  };

  const task = res?.tasks.find((t) => t.id === sel);
  const waves = res ? Math.max(0, ...res.tasks.map((t) => t.wave)) + 1 : 0;

  return (
    <Page title="Template Lab" sub="Dry-run one order item: nothing is published and no network element is called">
      <Hint>
        The Lab resolves the spec the way the orchestrator does (the tenant’s override first, then GLOBAL), validates the parameters, evaluates
        the <code>when</code> rules and keys, picks a network element using the tenant’s entitlements and renders every payload. Choose a
        draft to try catalog changes before exporting them.
      </Hint>
      <div className="grid-editor">
        <Card title="Input">
          <div className="row">
            <Field label="Tenant">
              <select value={tenant} onChange={(e) => setTenant(e.target.value)}>
                {tenants.data?.map((t) => (
                  <option key={t.tenant}>{t.tenant}</option>
                ))}
              </select>
            </Field>
            <Field label="Spec">
              <select value={spec} onChange={(e) => (setSpec(e.target.value), setVersion(""))}>
                {codes.map((c) => (
                  <option key={c}>{c}</option>
                ))}
              </select>
            </Field>
            <Field label={`Version (${owner})`}>
              <select value={version} onChange={(e) => setVersion(e.target.value)}>
                <option value="">latest active</option>
                {versions.map((v) => (
                  <option key={v}>{v}</option>
                ))}
              </select>
            </Field>
            <Field label="Catalog">
              <select value={draft} onChange={(e) => setDraft(e.target.value)}>
                <option value="">current catalog</option>
                {drafts.data?.map((d) => (
                  <option key={d.id} value={d.id}>
                    draft: {d.name}
                  </option>
                ))}
              </select>
            </Field>
          </div>
          <Field label="Parameters (JSON)">
            <textarea className="editor short" value={params} onChange={(e) => setParams(e.target.value)} spellCheck={false} aria-label="parameters" />
          </Field>
          {p.error && <p className="small" style={{ color: "var(--bad)" }}>{p.error}</p>}
          <details>
            <summary>Simulated NE responses (optional)</summary>
            <p className="small muted">
              Map of task id to the response a dependent template may read, for example <code>{`{"auth": {"authId": "a1"}}`}</code>.
            </p>
            <textarea className="editor short" value={results} onChange={(e) => setResults(e.target.value)} spellCheck={false} aria-label="results" />
            {r.error && <p className="small" style={{ color: "var(--bad)" }}>{r.error}</p>}
          </details>
          <div className="actions bar">
            <button className="primary" disabled={!spec || !!p.error || !!r.error || act.busy} onClick={run}>
              {act.busy ? "Running…" : "Run"}
            </button>
            <button disabled={!res?.valid} onClick={send}>
              Send as a real order…
            </button>
          </div>
          <ErrorBox error={act.error} />
        </Card>
        <Card title="Outcome">
          {res ? (
            <>
              <p>
                <Badge value={res.valid ? "valid" : "invalid"} tone={res.valid ? "ok" : "bad"} /> {res.spec} {res.version}{" "}
                {res.specTenant && (
                  <Badge value={res.specTenant === "GLOBAL" ? "GLOBAL spec" : `${res.specTenant} override`} tone={res.specTenant === "GLOBAL" ? "info" : "warn"} />
                )}
              </p>
              {res.problems.length > 0 && (
                <ul className="problems">
                  {res.problems.map((x) => (
                    <li key={x}>{x}</li>
                  ))}
                </ul>
              )}
              <dl className="kv">
                <dt>Mode / priority</dt>
                <dd>
                  {res.txMode} · {res.priority}
                </dd>
                <dt>Entity keys</dt>
                <dd className="mono">{res.entityKeys.join(", ") || "—"}</dd>
                <dt>Tasks</dt>
                <dd>
                  {res.tasks.filter((t) => t.applies).length} of {res.tasks.length} apply, in {waves} wave(s)
                </dd>
              </dl>
            </>
          ) : (
            <p className="muted">Run the Lab to see the decomposition.</p>
          )}
        </Card>
      </div>
      {res && res.tasks.length > 0 && (
        <>
          <Card title="Decomposition (click a task)">
            <Dag
              nodes={res.tasks.map((t) => ({
                id: t.id,
                title: `${t.id}${t.applies ? "" : " (skipped)"}`,
                sub: `${t.op} → ${t.ne ?? "no NE"}`,
                tone: tone(t),
                deps: t.dependsOn ?? [],
                dashed: !t.applies,
              }))}
              selected={sel}
              onSelect={setSel}
            />
            <p className="small muted">Green renders cleanly, red has a problem, grey is skipped by its “when” rule, amber is the pivot.</p>
          </Card>
          <Card title={task ? `${task.id} · ${task.op}` : "Task"}>
            {task && (
              <>
                <Tabs tabs={views} value={view} onChange={setView} />
                {view === "Payload" && (
                  <>
                    <p className="small mono">{task.template ?? "no template"}</p>
                    <ErrorBox error={task.renderError ?? ""} />
                    <Json value={task.request} empty="(no body)" />
                  </>
                )}
                {view === "Compensation" && (
                  <>
                    <p className="small mono">
                      {task.compensate ?? "no compensation"} {task.compensateTemplate && `· ${task.compensateTemplate}`}
                    </p>
                    <ErrorBox error={task.compensateError ?? ""} />
                    <Json value={task.compensateRequest} empty="(no body)" />
                  </>
                )}
                {view === "Selection" && (
                  <dl className="kv">
                    <dt>Applies</dt>
                    <dd>
                      {task.applies ? "yes" : "no"} {task.when && <span className="mono">({task.when})</span>}
                    </dd>
                    {task.whenError && (
                      <>
                        <dt>Rule error</dt>
                        <dd>{task.whenError}</dd>
                      </>
                    )}
                    <dt>Wave</dt>
                    <dd>{task.wave + 1}</dd>
                    <dt>NE type</dt>
                    <dd className="mono">{task.nfType}</dd>
                    <dt>Selected NE</dt>
                    <dd>
                      {task.ne ? `${task.ne} (owner ${task.neTenant}${task.shared ? ", shared" : ""})` : <Badge value="none" tone="bad" />}
                    </dd>
                    {task.neError && (
                      <>
                        <dt>Selection error</dt>
                        <dd>{task.neError}</dd>
                      </>
                    )}
                    <dt>Subscriber group</dt>
                    <dd>{task.subscriberGroup || "—"}</dd>
                    <dt>Coalesce key</dt>
                    <dd className="mono">{task.coalesceKey || "—"}</dd>
                    <dt>Depends on</dt>
                    <dd>{task.dependsOn.join(", ") || "—"}</dd>
                  </dl>
                )}
              </>
            )}
          </Card>
        </>
      )}
      {res?.order !== undefined && (
        <Card title="TMF641 service order this item becomes">
          <Json value={res.order} />
        </Card>
      )}
    </Page>
  );
}
