"use client";

import { Badge, Card, ErrorBox, Page, Stat, Table } from "@/components/ui";
import { fmtNum } from "@/lib/format";
import { useApi } from "@/lib/hooks";
import type { Kafka } from "@/lib/types";

// topicParts splits dxps.task.<domain>.p<n> / dxps.bc.p<n> into a family and a priority.
function topicParts(t: string): { family: string; prio: string } {
  const m = /^(.*)\.(p[0-3])$/.exec(t);
  return m ? { family: m[1], prio: m[2].toUpperCase() } : { family: t, prio: "" };
}

export default function KafkaPage() {
  const k = useApi<Kafka>("kafka", 5000);
  const topics = k.data?.topics ?? [];
  const families = new Map<string, Record<string, number>>();
  for (const t of topics) {
    const p = topicParts(t.topic);
    const row = families.get(p.family) ?? {};
    row[p.prio || "—"] = t.end;
    families.set(p.family, row);
  }
  const lag = (k.data?.lags ?? []).reduce((a, l) => a + l.lag, 0);
  const max = Math.max(1, ...topics.map((t) => t.end));

  return (
    <Page title="Kafka" sub="Priority topics, their end offsets, and consumer group lag — refreshes every 5 seconds">
      <ErrorBox error={k.error} />
      <div className="stats">
        <Stat label="Topics" value={topics.length} />
        <Stat label="Messages (sum of end offsets)" value={fmtNum(topics.reduce((a, t) => a + t.end, 0))} />
        <Stat label="Consumer groups" value={k.data?.lags.length ?? 0} />
        <Stat label="Total lag" value={fmtNum(lag)} tone={lag > 1000 ? "warn" : "ok"} />
      </div>
      <div className="grid2">
        <Card title="Topics by priority lane">
          <Table
            rows={[...families.entries()].map(([family, r]) => ({ family, r }))}
            rowKey={(r) => r.family}
            columns={[
              { key: "family", label: "Topic family", render: (r) => <span className="mono">{r.family}</span> },
              ...["P0", "P1", "P2", "P3", "—"].map((p) => ({
                key: p,
                label: p === "—" ? "single" : p,
                align: "right" as const,
                render: (r: { r: Record<string, number> }) => (r.r[p] === undefined ? "" : fmtNum(r.r[p])),
              })),
            ]}
          />
          <p className="small muted">P0 is the most urgent lane. Each lane has its own consumers so bulk work cannot starve urgent orders.</p>
        </Card>
        <Card title="Consumer groups">
          <Table
            rows={[...(k.data?.lags ?? [])].sort((a, b) => b.lag - a.lag || a.group.localeCompare(b.group))}
            rowKey={(g) => g.group}
            columns={[
              { key: "group", label: "Group", render: (g) => <span className="mono">{g.group}</span> },
              { key: "state", label: "State", render: (g) => <Badge value={g.state} tone={g.state.startsWith("Stable") ? "ok" : g.state === "Empty" ? "muted" : "warn"} /> },
              { key: "lag", label: "Lag", align: "right", render: (g) => (g.lag ? <Badge value={fmtNum(g.lag)} tone="warn" /> : "0") },
            ]}
          />
        </Card>
      </div>
      <Card title="Topic volume">
        <Table
          rows={topics.filter((t) => t.end > 0)}
          rowKey={(t) => t.topic}
          columns={[
            { key: "topic", label: "Topic", render: (t) => <span className="mono">{t.topic}</span> },
            {
              key: "bar",
              label: "Volume (log scale)",
              render: (t) => (
                <div className="vbar-wrap">
                  <div className="vbar" style={{ width: `${(100 * Math.log10(t.end + 1)) / Math.log10(max + 1)}%` }} />
                </div>
              ),
            },
            { key: "end", label: "End offset", align: "right", render: (t) => fmtNum(t.end) },
          ]}
        />
      </Card>
    </Page>
  );
}
