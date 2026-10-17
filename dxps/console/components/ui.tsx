"use client";

import type { ReactNode } from "react";
import { toneOf } from "@/lib/format";

export function Badge({ value, tone }: { value: string; tone?: string }) {
  return <span className={`badge ${tone ?? toneOf(value)}`}>{value}</span>;
}

export function ErrorBox({ error }: { error: string }) {
  return error ? (
    <div className="alert bad" role="alert">
      {error}
    </div>
  ) : null;
}

export function Notice({ text }: { text: string }) {
  return text ? (
    <div className="alert ok" role="status">
      {text}
    </div>
  ) : null;
}

export function Hint({ children }: { children: ReactNode }) {
  return <div className="alert info">{children}</div>;
}

export function Page({ title, sub, actions, children }: { title: string; sub?: ReactNode; actions?: ReactNode; children: ReactNode }) {
  return (
    <section>
      <header className="page-head">
        <div>
          <h1>{title}</h1>
          {sub && <p className="muted sub">{sub}</p>}
        </div>
        <div className="actions">{actions}</div>
      </header>
      {children}
    </section>
  );
}

export function Card({ title, actions, children }: { title?: string; actions?: ReactNode; children: ReactNode }) {
  return (
    <div className="card">
      {(title || actions) && (
        <div className="card-head">
          {title && <h2>{title}</h2>}
          {actions && <div className="actions">{actions}</div>}
        </div>
      )}
      {children}
    </div>
  );
}

export interface Column<T> {
  key: string;
  label: string;
  render?: (row: T) => ReactNode;
  align?: "right";
}

export function Table<T>({ rows, columns, empty = "Nothing to show", rowKey, onRow, selected }: {
  rows: T[] | undefined;
  columns: Column<T>[];
  empty?: string;
  rowKey: (row: T, i: number) => string;
  onRow?: (row: T) => void;
  selected?: string;
}) {
  if (!rows) return <p className="muted">Loading…</p>;
  if (rows.length === 0) return <p className="muted">{empty}</p>;
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            {columns.map((c) => (
              <th key={c.key} className={c.align}>
                {c.label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => {
            const k = rowKey(r, i);
            return (
              <tr key={k} onClick={onRow ? () => onRow(r) : undefined} className={`${onRow ? "clickable" : ""} ${selected === k ? "selected" : ""}`}>
                {columns.map((c) => (
                  <td key={c.key} className={c.align}>
                    {c.render ? c.render(r) : String((r as Record<string, unknown>)[c.key] ?? "")}
                  </td>
                ))}
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

export function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="field">
      <span>{label}</span>
      {children}
    </label>
  );
}

export function Stat({ label, value, tone = "", hint }: { label: string; value: ReactNode; tone?: string; hint?: string }) {
  return (
    <div className={`stat ${tone}`} title={hint}>
      <div className="stat-value">{value}</div>
      <div className="stat-label">{label}</div>
    </div>
  );
}

export function Json({ value, empty = "—" }: { value: unknown; empty?: string }) {
  if (value === undefined || value === null) return <span className="muted">{empty}</span>;
  return <pre className="json">{typeof value === "string" ? value : JSON.stringify(value, null, 2)}</pre>;
}

export function Code({ text }: { text: string }) {
  return <pre className="code">{text}</pre>;
}

export function Tabs<T extends string>({ tabs, value, onChange }: { tabs: readonly T[]; value: T; onChange: (t: T) => void }) {
  return (
    <div className="tabs" role="tablist">
      {tabs.map((t) => (
        <button key={t} role="tab" aria-selected={t === value} className={t === value ? "active" : ""} onClick={() => onChange(t)}>
          {t}
        </button>
      ))}
    </div>
  );
}

export function Dot({ up, label }: { up: boolean | undefined; label: string }) {
  const tone = up === undefined ? "muted" : up ? "ok" : "bad";
  return (
    <span className={`dot-label ${tone}`}>
      <span className={`dot ${tone}`} aria-hidden /> {label}
    </span>
  );
}

// StackBar draws one horizontal bar split by segments (value, tone, label).
export function StackBar({ parts, total }: { parts: { label: string; n: number; tone: string }[]; total?: number }) {
  const sum = total ?? parts.reduce((a, p) => a + p.n, 0);
  return (
    <div className="stackbar" role="img" aria-label={parts.map((p) => `${p.label} ${p.n}`).join(", ")}>
      {parts
        .filter((p) => p.n > 0)
        .map((p) => (
          <span key={p.label} className={`seg ${p.tone}`} style={{ width: `${(100 * p.n) / Math.max(sum, 1)}%` }} title={`${p.label}: ${p.n}`} />
        ))}
    </div>
  );
}

export function Spark({ values, label, height = 36 }: { values: number[]; label: string; height?: number }) {
  const w = 220;
  if (values.length < 2) return <div className="muted small">{label}: not enough samples yet</div>;
  const max = Math.max(...values, 1);
  const pts = values.map((v, i) => `${((i / (values.length - 1)) * w).toFixed(1)},${(height - 2 - (v / max) * (height - 4)).toFixed(1)}`);
  return (
    <figure className="spark">
      <svg width={w} height={height} viewBox={`0 0 ${w} ${height}`} role="img" aria-label={label}>
        <polyline points={pts.join(" ")} fill="none" stroke="currentColor" strokeWidth="1.6" />
      </svg>
      <figcaption>
        {label} <b>{values[values.length - 1]}</b> <span className="muted">(max {max})</span>
      </figcaption>
    </figure>
  );
}

export interface DagNode {
  id: string;
  title: string;
  sub?: string;
  tone?: string;
  deps: string[];
  dashed?: boolean;
}

// Dag lays nodes out left to right by dependency depth and draws the edges.
export function Dag({ nodes, selected, onSelect }: { nodes: DagNode[]; selected?: string; onSelect?: (id: string) => void }) {
  const W = 190;
  const H = 52;
  const GX = 56;
  const GY = 16;
  const byId = new Map(nodes.map((n) => [n.id, n]));
  const depth = new Map<string, number>();
  const level = (id: string, seen = new Set<string>()): number => {
    if (depth.has(id)) return depth.get(id)!;
    if (seen.has(id)) return 0;
    seen.add(id);
    const n = byId.get(id);
    const d = n ? Math.max(-1, ...n.deps.filter((x) => byId.has(x)).map((x) => level(x, seen))) + 1 : 0;
    depth.set(id, d);
    return d;
  };
  nodes.forEach((n) => level(n.id));
  const cols: string[][] = [];
  nodes.forEach((n) => {
    const d = depth.get(n.id)!;
    (cols[d] ??= []).push(n.id);
  });
  const pos = new Map<string, { x: number; y: number }>();
  cols.forEach((c, i) => c.forEach((id, j) => pos.set(id, { x: 8 + i * (W + GX), y: 8 + j * (H + GY) })));
  const width = 16 + cols.length * (W + GX) - GX;
  const height = 16 + Math.max(1, ...cols.map((c) => c.length)) * (H + GY) - GY;
  if (!nodes.length) return <p className="muted">No tasks</p>;
  return (
    <div className="dag-wrap">
      <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} className="dag" role="img" aria-label="task graph">
        <defs>
          <marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
            <path d="M 0 0 L 10 5 L 0 10 z" fill="#8a97a6" />
          </marker>
        </defs>
        {nodes.flatMap((n) =>
          n.deps
            .filter((d) => pos.has(d))
            .map((d) => {
              const a = pos.get(d)!;
              const b = pos.get(n.id)!;
              const x1 = a.x + W;
              const y1 = a.y + H / 2;
              const x2 = b.x;
              const y2 = b.y + H / 2;
              const mx = (x1 + x2) / 2;
              return (
                <path key={`${d}-${n.id}`} d={`M ${x1} ${y1} C ${mx} ${y1}, ${mx} ${y2}, ${x2 - 2} ${y2}`} fill="none" stroke="#8a97a6" strokeWidth="1.4" markerEnd="url(#arrow)" />
              );
            }),
        )}
        {nodes.map((n) => {
          const p = pos.get(n.id)!;
          return (
            <g
              key={n.id}
              transform={`translate(${p.x},${p.y})`}
              className={`dag-node ${n.tone ?? "muted"} ${selected === n.id ? "sel" : ""} ${onSelect ? "clickable" : ""}`}
              onClick={onSelect ? () => onSelect(n.id) : undefined}
              role={onSelect ? "button" : undefined}
              aria-label={n.title}
            >
              <rect width={W} height={H} rx="7" strokeDasharray={n.dashed ? "4 3" : undefined} />
              <text x="10" y="20" className="t1">
                {n.title.length > 26 ? n.title.slice(0, 25) + "…" : n.title}
              </text>
              {n.sub && (
                <text x="10" y="38" className="t2">
                  {n.sub.length > 30 ? n.sub.slice(0, 29) + "…" : n.sub}
                </text>
              )}
            </g>
          );
        })}
      </svg>
    </div>
  );
}

export function Pager({ offset, limit, total, onChange }: { offset: number; limit: number; total: number; onChange: (o: number) => void }) {
  return (
    <div className="pager">
      <button disabled={offset === 0} onClick={() => onChange(Math.max(0, offset - limit))}>
        Previous
      </button>
      <span className="muted">
        {total ? `${offset + 1}–${Math.min(offset + limit, total)} of ${total.toLocaleString("en-US")}` : "0 results"}
      </span>
      <button disabled={offset + limit >= total} onClick={() => onChange(offset + limit)}>
        Next
      </button>
    </div>
  );
}
