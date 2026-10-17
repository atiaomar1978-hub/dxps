"use client";

import { useState } from "react";
import { Can } from "@/components/Shell";
import { Badge, Card, ErrorBox, Field, Hint, Notice, Page, Table } from "@/components/ui";
import { api } from "@/lib/api";
import { fmtNum } from "@/lib/format";
import { useAction, useApi } from "@/lib/hooks";
import type { Access, NE, Tenant } from "@/lib/types";

export default function Inventory() {
  const nes = useApi<NE[]>("nes");
  const tenants = useApi<Tenant[]>("tenants");
  const [key, setKey] = useState("");
  const [lim, setLim] = useState({ maxTps: 1, maxConcurrency: 1, reservedPct: 0 });
  const [acc, setAcc] = useState({ tenant: "", quotaTps: 100, allowedOps: "", subscriberGroup: "" });
  const act = useAction();
  const ne = nes.data?.find((n) => n.tenant + "/" + n.code === key);

  const pick = (n: NE) => {
    setKey(n.tenant + "/" + n.code);
    setLim({ maxTps: n.maxTps, maxConcurrency: n.maxConcurrency, reservedPct: n.reservedPct });
    setAcc({ tenant: "", quotaTps: 100, allowedOps: `${n.domain === "sba" ? n.nfType.toLowerCase() : n.domain}.*`, subscriberGroup: "" });
  };
  const editAccess = (a: Access) => setAcc({ tenant: a.tenant, quotaTps: a.quotaTps, allowedOps: a.allowedOps.join(", "), subscriberGroup: a.subscriberGroup ?? "" });

  const saveLimits = () =>
    act.run(async () => {
      if (!ne) return;
      await api(`nes/${ne.tenant}/${ne.code}`, { method: "PUT", body: lim });
      nes.reload();
      return `${ne.code} limits saved`;
    });
  const saveAccess = () =>
    act.run(async () => {
      if (!ne) return;
      const allowedOps = acc.allowedOps.split(/[\s,]+/).filter(Boolean);
      await api(`nes/${ne.tenant}/${ne.code}/access`, {
        method: "PUT",
        body: { tenant: acc.tenant, quotaTps: acc.quotaTps, allowedOps, subscriberGroup: acc.subscriberGroup || undefined },
      });
      nes.reload();
      return `${acc.tenant} entitlement on ${ne.code} saved`;
    });

  return (
    <Page title="NE inventory" sub="Capacity limits of each network element and which tenants may use shared elements">
      <Hint>
        <b>Max TPS</b> and <b>concurrency</b> protect the element; <b>reserved %</b> is kept for its owner. On a shared element, each other
        tenant needs an entitlement: its own quota and the operations it may run (patterns such as <code>udr.*</code>). Changes apply within
        about 10 seconds; <code>run-all.sh</code> re-seeds the demo inventory on start.
      </Hint>
      <ErrorBox error={nes.error || act.error} />
      <Notice text={act.message} />
      <div className="grid-editor">
        <Card>
          <Table
            rows={nes.data}
            rowKey={(n) => n.tenant + "/" + n.code}
            selected={key}
            onRow={pick}
            columns={[
              { key: "code", label: "NE", render: (n) => <b>{n.code}</b> },
              { key: "tenant", label: "Owner" },
              { key: "nfType", label: "NF type", render: (n) => <span className="mono">{n.nfType}</span> },
              { key: "maxTps", label: "Max TPS", align: "right", render: (n) => fmtNum(n.maxTps) },
              { key: "maxConcurrency", label: "Concurrency", align: "right" },
              { key: "reservedPct", label: "Reserved", align: "right", render: (n) => `${n.reservedPct}%` },
              {
                key: "access",
                label: "Entitled tenants",
                render: (n) =>
                  n.shared ? (
                    <div className="chips">
                      {Object.values(n.access ?? {}).map((a) => (
                        <span key={a.tenant} className="chip">
                          {a.tenant} · {a.quotaTps} TPS
                        </span>
                      ))}
                    </div>
                  ) : (
                    <span className="muted">dedicated</span>
                  ),
              },
            ]}
          />
        </Card>
        <div>
          {ne ? (
            <>
              <Card title={`${ne.code} limits`}>
                <Can role="configurator">
                  <div className="row">
                    <Field label="Max TPS">
                      <input type="number" min={1} max={100000} value={lim.maxTps} onChange={(e) => setLim({ ...lim, maxTps: Number(e.target.value) })} />
                    </Field>
                    <Field label="Concurrency">
                      <input type="number" min={1} max={10000} value={lim.maxConcurrency} onChange={(e) => setLim({ ...lim, maxConcurrency: Number(e.target.value) })} />
                    </Field>
                    <Field label="Reserved %">
                      <input type="number" min={0} max={90} value={lim.reservedPct} onChange={(e) => setLim({ ...lim, reservedPct: Number(e.target.value) })} />
                    </Field>
                    <button className="primary" disabled={act.busy} onClick={saveLimits}>
                      Save limits
                    </button>
                  </div>
                </Can>
              </Card>
              <Card title="Entitlements">
                {ne.shared ? (
                  <>
                    <Table
                      rows={Object.values(ne.access ?? {})}
                      empty="No tenant is entitled yet"
                      rowKey={(a) => a.tenant}
                      onRow={editAccess}
                      columns={[
                        { key: "tenant", label: "Tenant" },
                        { key: "quotaTps", label: "Quota TPS", align: "right" },
                        { key: "allowedOps", label: "Operations", render: (a) => <span className="mono small">{a.allowedOps.join(", ")}</span> },
                        { key: "subscriberGroup", label: "Group", render: (a) => a.subscriberGroup ?? "" },
                      ]}
                    />
                    <Can role="configurator">
                      <div className="stack" style={{ marginTop: 12 }}>
                        <div className="row">
                          <Field label="Tenant">
                            <select value={acc.tenant} onChange={(e) => setAcc({ ...acc, tenant: e.target.value })}>
                              <option value="">Choose…</option>
                              {tenants.data
                                ?.filter((t) => t.tenant !== ne.tenant)
                                .map((t) => (
                                  <option key={t.tenant}>{t.tenant}</option>
                                ))}
                            </select>
                          </Field>
                          <Field label="Quota TPS">
                            <input type="number" min={1} max={100000} value={acc.quotaTps} onChange={(e) => setAcc({ ...acc, quotaTps: Number(e.target.value) })} />
                          </Field>
                        </div>
                        <Field label="Allowed operations (comma separated)">
                          <input value={acc.allowedOps} onChange={(e) => setAcc({ ...acc, allowedOps: e.target.value })} />
                        </Field>
                        <Field label="Subscriber group (optional)">
                          <input value={acc.subscriberGroup} onChange={(e) => setAcc({ ...acc, subscriberGroup: e.target.value })} />
                        </Field>
                        <button className="primary" disabled={!acc.tenant || !acc.allowedOps.trim() || act.busy} onClick={saveAccess}>
                          {ne.access?.[acc.tenant] ? "Update entitlement" : "Grant entitlement"}
                        </button>
                      </div>
                    </Can>
                  </>
                ) : (
                  <p className="muted">
                    <Badge value="dedicated" /> Only {ne.tenant} uses this element.
                  </p>
                )}
              </Card>
            </>
          ) : (
            <Card>
              <p className="muted">Select a network element.</p>
            </Card>
          )}
        </div>
      </div>
    </Page>
  );
}
