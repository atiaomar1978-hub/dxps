"use client";

import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import { Can, useSession } from "@/components/Shell";
import { Badge, Card, Code, ErrorBox, Field, Notice, Page, Table } from "@/components/ui";
import { api, qs } from "@/lib/api";
import { allows, fmtTime } from "@/lib/format";
import { useAction, useApi } from "@/lib/hooks";
import type { Draft, Validation } from "@/lib/types";

interface Export {
  name: string;
  files: Record<string, string>;
  deleted?: string[];
  target: string;
}

const pathRule = /^(specs\/[A-Za-z0-9_.-]+\.yaml|templates\/[a-z0-9_-]+\/[A-Za-z0-9_.-]+\.json\.tmpl)$/;

export default function DraftEditor() {
  const { id } = useParams<{ id: string }>();
  const router = useRouter();
  const me = useSession();
  const canEdit = allows(me.roles, "configurator");
  const d = useApi<{ draft: Draft; base: Record<string, string> }>(`drafts/${id}`);
  const catalogFiles = useApi<Record<string, string>>("catalog/files");
  const [files, setFiles] = useState<Record<string, string>>({});
  const [deleted, setDeleted] = useState<string[]>([]);
  const [open, setOpen] = useState("");
  const [dirty, setDirty] = useState(false);
  const [add, setAdd] = useState("");
  const [newPath, setNewPath] = useState("");
  const [val, setVal] = useState<Validation>();
  const [exp, setExp] = useState<Export>();
  const act = useAction();

  useEffect(() => {
    if (!d.data) return;
    setFiles(d.data.draft.files);
    setDeleted(d.data.draft.deleted ?? []);
    setOpen((o) => o || Object.keys(d.data!.draft.files).sort()[0] || "");
    setDirty(false);
  }, [d.data]);

  const base = d.data?.base ?? {};
  const names = Object.keys(files).sort();
  const edit = (text: string) => {
    setFiles({ ...files, [open]: text });
    setDirty(true);
  };
  const save = () =>
    act.run(async () => {
      await api(`drafts/${id}`, { method: "PUT", body: { files, deleted } });
      setDirty(false);
      return "Draft saved";
    });
  const validate = () =>
    act.run(async () => {
      if (dirty) await api(`drafts/${id}`, { method: "PUT", body: { files, deleted } });
      setDirty(false);
      const v = await api<Validation>(`drafts/${id}/validate`, { body: {} });
      setVal(v);
      return v.ok ? "The catalog loads with this draft" : "";
    });
  const doExport = () =>
    act.run(async () => {
      if (dirty) await api(`drafts/${id}`, { method: "PUT", body: { files, deleted } });
      setDirty(false);
      setExp(await api<Export>(`drafts/${id}/export`, { body: {} }));
    });
  const remove = () =>
    act.run(async () => {
      if (!window.confirm("Delete this draft?")) return;
      await api(`drafts/${id}`, { method: "DELETE" });
      router.push("/studio");
    });
  const addFile = (p: string, text: string) => {
    setFiles({ ...files, [p]: text });
    setOpen(p);
    setDirty(true);
  };
  const download = () => {
    if (!exp) return;
    const blob = new Blob([JSON.stringify(exp, null, 2)], { type: "application/json" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `dxps-catalog-draft-${id.slice(0, 8)}.json`;
    a.click();
    URL.revokeObjectURL(a.href);
  };

  return (
    <Page
      title={d.data?.draft.name ?? "Draft"}
      sub={
        <>
          <Link href="/studio">Catalog studio</Link> / {d.data && `by ${d.data.draft.author} · updated ${fmtTime(d.data.draft.updated)}`}
        </>
      }
      actions={
        <Can role="configurator">
          <button className="primary" disabled={!dirty || act.busy} onClick={save}>
            Save
          </button>
          <button disabled={act.busy} onClick={validate}>
            Validate
          </button>
          <Link className="button" href={`/lab${qs({ draft: id })}`}>
            Open in Template Lab
          </Link>
          <button disabled={act.busy} onClick={doExport}>
            Export
          </button>
          <button className="danger" disabled={act.busy} onClick={remove}>
            Delete
          </button>
        </Can>
      }
    >
      <ErrorBox error={d.error || act.error} />
      <Notice text={act.message} />
      <div className="grid-side">
        <div>
          <Card title="Files">
            <ul className="files">
              {names.map((n) => (
                <li key={n} className={`${n === open ? "active" : ""} ${files[n] !== base[n] ? "changed" : ""}`} onClick={() => setOpen(n)}>
                  {n}
                  {base[n] === undefined && <Badge value="new" tone="info" />}
                </li>
              ))}
              {deleted.map((n) => (
                <li key={n} className="removed" title="removed from the catalog when exported">
                  {n}
                </li>
              ))}
            </ul>
            <p className="small muted">• changed compared with the catalog</p>
          </Card>
          {canEdit && (
            <Card title="Add">
              <div className="stack">
                <Field label="Catalog file">
                  <select value={add} onChange={(e) => setAdd(e.target.value)}>
                    <option value="">Choose…</option>
                    {Object.keys(catalogFiles.data ?? {})
                      .filter((p) => !files[p])
                      .sort()
                      .map((p) => (
                        <option key={p}>{p}</option>
                      ))}
                  </select>
                </Field>
                <button disabled={!add} onClick={() => (addFile(add, catalogFiles.data?.[add] ?? ""), setAdd(""))}>
                  Add to draft
                </button>
                <Field label="New file path">
                  <input value={newPath} onChange={(e) => setNewPath(e.target.value)} placeholder="templates/sba/my.json.tmpl" />
                </Field>
                <button disabled={!pathRule.test(newPath) || !!files[newPath]} onClick={() => (addFile(newPath, ""), setNewPath(""))}>
                  Create file
                </button>
                {open && base[open] !== undefined && !deleted.includes(open) && (
                  <button
                    className="danger"
                    onClick={() => {
                      const f = { ...files };
                      delete f[open];
                      setFiles(f);
                      setDeleted([...deleted, open]);
                      setOpen(Object.keys(f).sort()[0] ?? "");
                      setDirty(true);
                    }}
                  >
                    Remove {open.split("/").pop()} from catalog
                  </button>
                )}
              </div>
            </Card>
          )}
        </div>
        <div>
          <Card title={open || "No file open"} actions={dirty ? <Badge value="unsaved" tone="warn" /> : undefined}>
            {open ? (
              <textarea className="editor" value={files[open] ?? ""} readOnly={!canEdit} spellCheck={false} onChange={(e) => edit(e.target.value)} aria-label="file editor" />
            ) : (
              <p className="muted">Add a file to the draft.</p>
            )}
          </Card>
          {val && (
            <Card title="Validation" actions={<Badge value={val.ok ? "catalog loads" : "does not load"} tone={val.ok ? "ok" : "bad"} />}>
              <ErrorBox error={val.error ?? ""} />
              {val.ok && (
                <Table
                  rows={val.smoke}
                  empty="No spec in the catalog has an example to smoke-test"
                  rowKey={(r) => `${r.tenant}|${r.spec}|${r.version}`}
                  columns={[
                    { key: "spec", label: "Spec", render: (r) => `${r.spec} ${r.version}` },
                    { key: "tenant", label: "Run as tenant" },
                    { key: "ok", label: "Example run", render: (r) => <Badge value={r.ok ? "pass" : "fail"} /> },
                    { key: "problems", label: "Problems", render: (r) => <span className="small">{r.problems?.join("; ")}</span> },
                  ]}
                />
              )}
            </Card>
          )}
          {exp && (
            <Card title="Export" actions={<button onClick={download}>Download bundle</button>}>
              <p className="small">
                Copy these files into <code>{exp.target}</code> (paths are relative to it), rebuild and restart the services. Only files that
                differ from the current catalog are listed.
              </p>
              {exp.deleted?.length ? <p className="small">Delete: {exp.deleted.join(", ")}</p> : null}
              {Object.entries(exp.files).length === 0 && <p className="muted">No changes compared with the catalog.</p>}
              {Object.entries(exp.files).map(([p, t]) => (
                <details key={p} open={Object.keys(exp.files).length === 1}>
                  <summary className="mono">{p}</summary>
                  <Code text={t} />
                </details>
              ))}
            </Card>
          )}
        </div>
      </div>
    </Page>
  );
}
