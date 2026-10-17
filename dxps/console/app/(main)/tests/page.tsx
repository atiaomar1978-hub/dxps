"use client";

import { useState } from "react";
import { Badge, Card, ErrorBox, Field, Hint, Page, Stat, Table } from "@/components/ui";
import { fmtNum, fmtTime } from "@/lib/format";
import { useApi } from "@/lib/hooks";
import type { TestReport } from "@/lib/types";

export default function Tests() {
  const t = useApi<TestReport>("tests");
  const [only, setOnly] = useState("fail");
  const d = t.data;
  if (d && d.available === false) {
    return (
      <Page title="Test results" sub="Unit and integration test run of the platform">
        <Hint>
          No test report yet. Run the platform test suite (it writes <code>~/dxps-runtime/results/tests.json</code>) and reload this page.
        </Hint>
      </Page>
    );
  }
  const results = (d?.results ?? []).filter((r) => !only || r.action === only);
  return (
    <Page title="Test results" sub={d?.generatedAt ? `Report generated ${fmtTime(d.generatedAt)}` : "Unit and integration test run of the platform"}>
      <ErrorBox error={t.error} />
      <div className="stats">
        <Stat label="Tests" value={fmtNum(d?.totals?.tests ?? 0)} />
        <Stat label="Passed" value={fmtNum(d?.totals?.passed ?? 0)} tone="ok" />
        <Stat label="Failed" value={fmtNum(d?.totals?.failed ?? 0)} tone={d?.totals?.failed ? "bad" : "ok"} />
        <Stat label="Skipped" value={fmtNum(d?.totals?.skipped ?? 0)} />
        <Stat label="Coverage" value={d?.coverage ? `${d.coverage.toFixed(1)}%` : "—"} />
        <Stat label="Duration" value={d?.elapsedSec ? `${d.elapsedSec.toFixed(0)} s` : "—"} />
      </div>
      <Card title="Packages">
        <Table
          rows={d?.packages}
          rowKey={(p) => p.name}
          columns={[
            { key: "name", label: "Package", render: (p) => <span className="mono">{p.name}</span> },
            { key: "tests", label: "Tests", align: "right" },
            { key: "passed", label: "Passed", align: "right" },
            { key: "failed", label: "Failed", align: "right", render: (p) => (p.failed ? <Badge value={String(p.failed)} tone="bad" /> : "0") },
            { key: "skipped", label: "Skipped", align: "right" },
            { key: "coverage", label: "Coverage", align: "right", render: (p) => `${p.coverage.toFixed(1)}%` },
          ]}
        />
      </Card>
      <Card
        title="Tests"
        actions={
          <Field label="Show">
            <select value={only} onChange={(e) => setOnly(e.target.value)}>
              <option value="fail">failed</option>
              <option value="skip">skipped</option>
              <option value="pass">passed</option>
              <option value="">all</option>
            </select>
          </Field>
        }
      >
        <Table
          rows={results.slice(0, 500)}
          empty="No tests in this category"
          rowKey={(r) => r.package + r.test}
          columns={[
            { key: "package", label: "Package", render: (r) => <span className="mono small">{r.package}</span> },
            { key: "test", label: "Test", render: (r) => <span className="mono">{r.test}</span> },
            { key: "action", label: "Result", render: (r) => <Badge value={r.action} /> },
            { key: "elapsed", label: "Seconds", align: "right", render: (r) => r.elapsed.toFixed(2) },
          ]}
        />
      </Card>
    </Page>
  );
}
