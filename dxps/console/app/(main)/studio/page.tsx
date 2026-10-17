"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useState } from "react";
import { Can } from "@/components/Shell";
import { Card, ErrorBox, Field, Hint, Page, Table } from "@/components/ui";
import { api } from "@/lib/api";
import { fmtTime } from "@/lib/format";
import { useAction, useApi } from "@/lib/hooks";
import type { Draft, DraftSummary } from "@/lib/types";

const newSpec = `specs/new-service.yaml`;
const skeleton = `# One YAML document per spec; separate several with ---
tenant: GLOBAL
spec: NewService
version: 1.0.0
description: Describe what this service does
txMode: ATOMIC
priorityDefault: P2
params:
  required: [supi]
  properties:
    supi: {type: string, pattern: "^imsi-[0-9]{15}$"}
entityKeys: ["'supi:' + params.supi"]
tasks:
  - id: am
    ne: {nfType: UDR}
    domain: sba
    op: udr.amData.put
    template: sba/am-data.json.tmpl
    compensate: udr.amData.delete
example: {supi: imsi-416770000000001}
`;

export default function Studio() {
  const list = useApi<DraftSummary[]>("drafts");
  const files = useApi<Record<string, string>>("catalog/files");
  const router = useRouter();
  const [name, setName] = useState("");
  const [pick, setPick] = useState<string[]>([]);
  const [blank, setBlank] = useState(false);
  const act = useAction();

  const create = () =>
    act.run(async () => {
      const f: Record<string, string> = {};
      for (const p of pick) f[p] = files.data?.[p] ?? "";
      if (blank) f[newSpec] = skeleton;
      const d = await api<Draft>("drafts", { body: { name, files: f } });
      router.push(`/studio/${d.id}`);
    });

  return (
    <Page title="Catalog studio" sub="Draft changes to service specs and payload templates, validate them, then export">
      <Hint>
        Drafts never touch the running platform. <b>Validate</b> loads the whole catalog with your changes and runs each spec’s example through
        the Template Lab. <b>Export</b> gives you the changed files for <code>dxps/internal/catalog/data</code>; the services pick them up after a
        rebuild and restart.
      </Hint>
      <ErrorBox error={list.error || act.error} />
      <div className="grid-editor">
        <Card title="Drafts">
          <Table
            rows={list.data}
            empty="No drafts yet"
            rowKey={(d) => d.id}
            onRow={(d) => router.push(`/studio/${d.id}`)}
            columns={[
              { key: "name", label: "Name", render: (d) => <Link href={`/studio/${d.id}`}>{d.name}</Link> },
              { key: "author", label: "Author" },
              { key: "files", label: "Files", render: (d) => <span className="mono small">{d.files.join(", ")}</span> },
              { key: "updated", label: "Updated", render: (d) => fmtTime(d.updated) },
            ]}
          />
        </Card>
        <Can role="configurator">
          <Card title="New draft">
            <div className="stack">
              <Field label="Name">
                <input value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. Add roaming barring" />
              </Field>
              <Field label="Start from catalog files (Ctrl/Cmd-click for several)">
                <select multiple size={10} value={pick} onChange={(e) => setPick([...e.target.selectedOptions].map((o) => o.value))}>
                  {Object.keys(files.data ?? {})
                    .sort()
                    .map((p) => (
                      <option key={p}>{p}</option>
                    ))}
                </select>
              </Field>
              <label className="inline">
                <input type="checkbox" checked={blank} onChange={(e) => setBlank(e.target.checked)} /> add a new spec skeleton (
                <code>{newSpec}</code>)
              </label>
              <button className="primary" disabled={!name.trim() || (!pick.length && !blank) || act.busy} onClick={create}>
                Create draft
              </button>
            </div>
          </Card>
        </Can>
      </div>
    </Page>
  );
}
