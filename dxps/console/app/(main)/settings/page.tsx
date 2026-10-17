"use client";

import { Card, Dot, ErrorBox, Hint, Page } from "@/components/ui";
import { useApi } from "@/lib/hooks";
import type { Overview } from "@/lib/types";

interface Info {
  api: string;
  catalogDir: string;
  gateway: string;
  kafka: string[];
  mosaic: string;
  results: string;
  runtime: string;
  sessionHours: number;
  stateDir: string;
}

export default function Settings() {
  const s = useApi<Info>("settings");
  const o = useApi<Overview>("overview");
  const d = s.data;
  return (
    <Page title="Settings" sub="Where this console reads from and writes to">
      <ErrorBox error={s.error} />
      {d && (
        <div className="grid2">
          <Card title="Connections">
            <dl className="kv">
              <dt>Console API</dt>
              <dd className="mono">{d.api}</dd>
              <dt>TMF641 gateway</dt>
              <dd className="mono">{d.gateway}/tmf-api/serviceOrdering/v5</dd>
              <dt>Kafka</dt>
              <dd className="mono">{d.kafka.join(", ")}</dd>
              <dt>Load generator</dt>
              <dd className="mono">{d.mosaic}</dd>
              <dt>Database</dt>
              <dd>PostgreSQL (platform operations role, row-level security bypassed for cross-tenant views; writes limited to inventory and audit)</dd>
            </dl>
            <h3>Reachability</h3>
            <div className="health">
              {Object.entries(o.data?.up ?? {}).map(([k, v]) => (
                <Dot key={k} up={v} label={k} />
              ))}
            </div>
          </Card>
          <Card title="Files and sessions">
            <dl className="kv">
              <dt>Runtime</dt>
              <dd className="mono">{d.runtime}</dd>
              <dt>Catalog source</dt>
              <dd className="mono">{d.catalogDir}</dd>
              <dt>Console state</dt>
              <dd className="mono">{d.stateDir} (users, drafts, console audit)</dd>
              <dt>Test report</dt>
              <dd className="mono">{d.results}</dd>
              <dt>Session lifetime</dt>
              <dd>{d.sessionHours} hours</dd>
            </dl>
          </Card>
        </div>
      )}
      <Hint>
        The services run with the catalog compiled into them. Catalog drafts are therefore published by exporting the files into the catalog
        source and rebuilding; tenant, NE and entitlement edits are live.
      </Hint>
    </Page>
  );
}
