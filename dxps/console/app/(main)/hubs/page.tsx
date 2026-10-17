"use client";

import { Badge, Card, ErrorBox, Hint, Page, Stat, Table } from "@/components/ui";
import { fmtNum, fmtTime } from "@/lib/format";
import { useApi } from "@/lib/hooks";
import type { Hub, Overview } from "@/lib/types";

export default function Hubs() {
  const hubs = useApi<Hub[]>("hubs");
  const o = useApi<Overview>("overview", 10000);
  const eh = o.data?.services?.eventhub;
  return (
    <Page title="Webhooks" sub="TMF688 event subscriptions (hubs) registered by tenants, and delivery counters">
      <Hint>
        Tenants register hubs through the TMF688 API; each order state change is signed with the hub’s secret and delivered by the event hub.
        Secrets are stored encrypted and never shown.
      </Hint>
      <ErrorBox error={hubs.error} />
      <div className="stats">
        <Stat label="Hubs" value={hubs.data?.length ?? 0} />
        <Stat label="Delivered" value={fmtNum(eh?.delivered ?? 0)} tone="ok" />
        <Stat label="Failed deliveries" value={fmtNum(eh?.failed ?? 0)} tone={eh?.failed ? "bad" : "ok"} />
      </div>
      <Card>
        <Table
          rows={hubs.data}
          empty="No hub registered"
          rowKey={(h) => h.id}
          columns={[
            { key: "tenant", label: "Tenant" },
            { key: "callback", label: "Callback", render: (h) => <span className="mono">{h.callback}</span> },
            { key: "query", label: "Filter", render: (h) => <span className="mono small">{h.query || "all events"}</span> },
            { key: "secret", label: "Signing secret", render: (h) => <Badge value={h.secretRef ? "encrypted" : "none"} tone={h.secretRef ? "ok" : "warn"} /> },
            { key: "status", label: "Status", render: (h) => <Badge value={h.status} /> },
            { key: "createdAt", label: "Registered", render: (h) => fmtTime(h.createdAt) },
          ]}
        />
      </Card>
    </Page>
  );
}
