"use client";

import { useState, type FormEvent } from "react";
import { useSession } from "@/components/Shell";
import { Card, ErrorBox, Field, Hint, Notice, Page, Table } from "@/components/ui";
import { api } from "@/lib/api";
import { fmtTime, roles } from "@/lib/format";
import { useAction, useApi } from "@/lib/hooks";
import type { User } from "@/lib/types";

function RolePicker({ value, onChange }: { value: string[]; onChange: (v: string[]) => void }) {
  return (
    <span className="chips">
      {roles.map((r) => (
        <label key={r} className="inline">
          <input
            type="checkbox"
            aria-label={r}
            checked={value.includes(r)}
            onChange={(e) => onChange(e.target.checked ? [...value, r] : value.filter((x) => x !== r))}
          />{" "}
          {r}
        </label>
      ))}
    </span>
  );
}

export default function UsersPage() {
  const me = useSession();
  const us = useApi<User[]>("users");
  const act = useAction();
  const [form, setForm] = useState({ name: "", password: "", roles: ["viewer"] });

  const create = (e: FormEvent) => {
    e.preventDefault();
    act.run(async () => {
      await api("users", { body: form });
      setForm({ name: "", password: "", roles: ["viewer"] });
      us.reload();
      return `User ${form.name} created`;
    });
  };

  const update = (u: User, change: Partial<{ roles: string[]; disabled: boolean; password: string }>) =>
    act.run(async () => {
      await api(`users/${encodeURIComponent(u.name)}`, { method: "PUT", body: { roles: u.roles, disabled: u.disabled, ...change } });
      us.reload();
      return `User ${u.name} updated`;
    });

  const reset = (u: User) => {
    const pw = window.prompt(`New password for ${u.name} (at least 12 characters)`);
    if (pw) update(u, { password: pw });
  };

  const remove = (u: User) =>
    act.run(async () => {
      if (!window.confirm(`Delete user ${u.name}?`)) return;
      await api(`users/${encodeURIComponent(u.name)}`, { method: "DELETE" });
      us.reload();
      return `User ${u.name} deleted`;
    });

  return (
    <Page title="Users" sub="Console accounts and their roles">
      <Hint>
        <b>viewer</b> sees everything · <b>operator</b> also submits and cancels orders, injects faults, redrives dead letters and runs load ·{" "}
        <b>configurator</b> also edits tenants, NE limits, entitlements and catalog drafts and uses the Template Lab · <b>admin</b> also manages
        users. Each role includes the ones before it. Five wrong passwords lock an account for a while.
      </Hint>
      <ErrorBox error={us.error || act.error} />
      <Notice text={act.message} />
      <Card>
        <Table
          rows={us.data}
          rowKey={(u) => u.name}
          columns={[
            { key: "name", label: "User", render: (u) => <b>{u.name}</b> },
            { key: "roles", label: "Roles", render: (u) => <RolePicker value={u.roles} onChange={(r) => update(u, { roles: r })} /> },
            {
              key: "disabled",
              label: "Enabled",
              render: (u) => (
                <input type="checkbox" aria-label={`enable ${u.name}`} checked={!u.disabled} onChange={(e) => update(u, { disabled: !e.target.checked })} />
              ),
            },
            { key: "created", label: "Created", render: (u) => fmtTime(u.created) },
            { key: "last", label: "Last login", render: (u) => fmtTime(u.lastLogin) },
            {
              key: "ops",
              label: "",
              render: (u) => (
                <span className="actions">
                  <button className="link" onClick={() => reset(u)}>
                    reset password
                  </button>
                  {u.name !== me.username && (
                    <button className="link" style={{ color: "var(--bad)" }} onClick={() => remove(u)}>
                      delete
                    </button>
                  )}
                </span>
              ),
            },
          ]}
        />
      </Card>
      <Card title="Add user">
        <form className="filters" onSubmit={create}>
          <Field label="User name">
            <input value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} pattern="[a-z][a-z0-9._\-]{1,31}" required />
          </Field>
          <Field label="Initial password (12+ characters)">
            <input type="password" value={form.password} onChange={(e) => setForm({ ...form, password: e.target.value })} minLength={12} required />
          </Field>
          <RolePicker value={form.roles} onChange={(r) => setForm({ ...form, roles: r })} />
          <button className="primary" disabled={act.busy}>
            Create
          </button>
        </form>
      </Card>
    </Page>
  );
}
