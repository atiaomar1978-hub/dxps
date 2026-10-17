"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { Badge, Card, ErrorBox, Field, Hint, Json, Page } from "@/components/ui";
import { api, qs } from "@/lib/api";
import { composeKey, parseJson } from "@/lib/format";
import { useAction, useApi } from "@/lib/hooks";
import type { SpecDetail, SpecSummary, Tenant } from "@/lib/types";

interface Item {
  id: string;
  spec: string;
  action: string;
  params: string;
  dependsOn: string;
}

interface Sent {
  status: number;
  id: string;
  request: unknown;
  response: unknown;
}

const blank = (n: number, spec = ""): Item => ({ id: String(n), spec, action: "add", params: "{}", dependsOn: "" });

export default function Compose() {
  const tenants = useApi<Tenant[]>("tenants");
  const catalog = useApi<SpecSummary[]>("catalog");
  const [tenant, setTenant] = useState("host-mno");
  const [externalId, setExternalId] = useState("");
  const [priority, setPriority] = useState("");
  const [items, setItems] = useState<Item[]>([blank(1)]);
  const [sent, setSent] = useState<Sent>();
  const act = useAction();
  const codes = [...new Set((catalog.data ?? []).map((s) => s.spec))].sort();

  useEffect(() => {
    const raw = sessionStorage.getItem(composeKey);
    if (!raw) return;
    sessionStorage.removeItem(composeKey);
    const v = parseJson<{ tenant: string; items: { id: string; spec: string; action: string; params: unknown }[] }>(raw).value;
    if (!v) return;
    setTenant(v.tenant);
    setItems(v.items.map((i) => ({ id: i.id, spec: i.spec, action: i.action, params: JSON.stringify(i.params, null, 2), dependsOn: "" })));
  }, []);

  const set = (i: number, patch: Partial<Item>) => setItems(items.map((x, j) => (j === i ? { ...x, ...patch } : x)));
  const example = async (i: number) => {
    const it = items[i];
    const own = (catalog.data ?? []).filter((s) => s.spec === it.spec && (s.tenant === tenant || s.tenant === "GLOBAL"));
    const s = own.find((x) => x.tenant === tenant) ?? own[own.length - 1];
    if (!s) return;
    const d = await api<SpecDetail>(`catalog/spec${qs({ tenant: s.tenant, spec: s.spec, version: s.version })}`);
    set(i, { params: JSON.stringify(d.spec.example ?? {}, null, 2) });
  };
  const parsed = items.map((i) => parseJson<Record<string, unknown>>(i.params));
  const bad = parsed.some((p) => p.error) || items.some((i) => !i.spec);

  const submit = () =>
    act.run(async () => {
      const out = await api<Sent>("orders", {
        body: {
          tenant,
          externalId,
          priority,
          items: items.map((i, k) => ({
            id: i.id,
            spec: i.spec,
            action: i.action,
            params: parsed[k].value,
            dependsOn: i.dependsOn.split(",").map((x) => x.trim()).filter(Boolean),
          })),
        },
      });
      setSent(out);
    });

  return (
    <Page title="Order composer" sub="Build a TMF641 service order and submit it through the northbound gateway as the chosen tenant">
      <Hint>
        The order is sent to the real gateway with a short-lived token for the tenant (2 minutes, order read and write only), so it is
        provisioned on the simulators like any API order. The console user is recorded as the actor in both audit trails.
      </Hint>
      <ErrorBox error={act.error} />
      <div className="grid-editor">
        <div>
          <Card title="Order">
            <div className="row">
              <Field label="Tenant">
                <select value={tenant} onChange={(e) => setTenant(e.target.value)}>
                  {tenants.data?.map((t) => (
                    <option key={t.tenant} disabled={t.status !== "ACTIVE"}>
                      {t.tenant}
                    </option>
                  ))}
                </select>
              </Field>
              <Field label="External id">
                <input value={externalId} onChange={(e) => setExternalId(e.target.value)} placeholder="optional, e.g. CRM-123" />
              </Field>
              <Field label="Priority">
                <select value={priority} onChange={(e) => setPriority(e.target.value)}>
                  <option value="">spec default</option>
                  {["0", "1", "2", "3"].map((p) => (
                    <option key={p} value={p}>
                      P{p}
                    </option>
                  ))}
                </select>
              </Field>
            </div>
          </Card>
          {items.map((it, i) => (
            <Card
              key={i}
              title={`Item ${it.id}`}
              actions={
                items.length > 1 && (
                  <button className="danger small" onClick={() => setItems(items.filter((_, j) => j !== i))}>
                    Remove
                  </button>
                )
              }
            >
              <div className="row">
                <Field label="Item id">
                  <input value={it.id} onChange={(e) => set(i, { id: e.target.value })} style={{ width: 70 }} />
                </Field>
                <Field label="Service spec">
                  <select value={it.spec} onChange={(e) => set(i, { spec: e.target.value })}>
                    <option value="">Choose…</option>
                    {codes.map((c) => (
                      <option key={c}>{c}</option>
                    ))}
                  </select>
                </Field>
                <Field label="Action">
                  <select value={it.action} onChange={(e) => set(i, { action: e.target.value })}>
                    {["add", "modify", "delete"].map((a) => (
                      <option key={a}>{a}</option>
                    ))}
                  </select>
                </Field>
                <Field label="Depends on items">
                  <input value={it.dependsOn} onChange={(e) => set(i, { dependsOn: e.target.value })} placeholder="e.g. 1" style={{ width: 90 }} />
                </Field>
                <button disabled={!it.spec} onClick={() => example(i)}>
                  Use example
                </button>
              </div>
              <Field label="Characteristics (JSON object)">
                <textarea className="editor short" value={it.params} onChange={(e) => set(i, { params: e.target.value })} spellCheck={false} aria-label={`item ${it.id} parameters`} />
              </Field>
              {parsed[i].error && <p className="small" style={{ color: "var(--bad)" }}>{parsed[i].error}</p>}
            </Card>
          ))}
          <div className="actions bar">
            <button disabled={items.length >= 20} onClick={() => setItems([...items, blank(items.length + 1)])}>
              Add item
            </button>
            <button className="primary" disabled={bad || act.busy} onClick={submit}>
              {act.busy ? "Submitting…" : "Submit order"}
            </button>
          </div>
        </div>
        <Card title="Gateway response">
          {sent ? (
            <>
              <p>
                <Badge value={`HTTP ${sent.status}`} tone={sent.status < 300 ? "ok" : "bad"} />{" "}
                {sent.id && <Link href={`/orders/${tenant}/${sent.id}`}>Follow order {sent.id.slice(0, 8)} →</Link>}
              </p>
              <h3>Response</h3>
              <Json value={sent.response} />
              <h3>TMF641 request sent</h3>
              <Json value={sent.request} />
            </>
          ) : (
            <p className="muted">Submit the order to see the gateway’s answer.</p>
          )}
        </Card>
      </div>
    </Page>
  );
}
