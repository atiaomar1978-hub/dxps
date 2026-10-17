"use client";

import { useState, type FormEvent } from "react";
import { useSession } from "@/components/Shell";
import { Card, ErrorBox, Field, Notice, Page } from "@/components/ui";
import { api } from "@/lib/api";
import { useAction } from "@/lib/hooks";

export default function AccountPage() {
  const me = useSession();
  const act = useAction();
  const [form, setForm] = useState({ current: "", next: "", again: "" });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    act.run(async () => {
      if (form.next !== form.again) throw new Error("the new passwords differ");
      await api("auth/password", { body: { old: form.current, new: form.next } });
      setForm({ current: "", next: "", again: "" });
      return "Password changed";
    });
  };

  return (
    <Page title="My account">
      <Card>
        <p>
          Signed in as <b>{me.username}</b> with roles {me.roles.join(", ")}.
        </p>
      </Card>
      <Card title="Change password">
        <ErrorBox error={act.error} />
        <Notice text={act.message} />
        <form className="stack" onSubmit={submit}>
          <Field label="Current password">
            <input type="password" value={form.current} onChange={(e) => setForm({ ...form, current: e.target.value })} autoComplete="current-password" required />
          </Field>
          <Field label="New password (at least 12 characters)">
            <input type="password" value={form.next} onChange={(e) => setForm({ ...form, next: e.target.value })} autoComplete="new-password" minLength={12} required />
          </Field>
          <Field label="Repeat new password">
            <input type="password" value={form.again} onChange={(e) => setForm({ ...form, again: e.target.value })} autoComplete="new-password" required />
          </Field>
          <button className="primary" disabled={act.busy}>
            Change password
          </button>
        </form>
      </Card>
    </Page>
  );
}
