export const roles = ["viewer", "operator", "configurator", "admin"] as const;
export type Role = (typeof roles)[number];

// allows mirrors the API hierarchy: every role includes the ones before it.
export function allows(have: string[] | undefined, need: Role): boolean {
  const n = roles.indexOf(need);
  return (have ?? []).some((r) => roles.indexOf(r as Role) >= n);
}

export function fmtTime(t: string | null | undefined): string {
  if (!t) return "";
  const d = new Date(t);
  if (Number.isNaN(d.getTime()) || d.getUTCFullYear() < 1900) return "";
  return d.toISOString().replace("T", " ").replace(/\.\d+Z$/, "Z");
}

export function fmtNum(n: number | null | undefined): string {
  return n === null || n === undefined ? "" : n.toLocaleString("en-US");
}

export function fmtMs(ms: number | null | undefined): string {
  if (ms === null || ms === undefined) return "";
  if (ms < 1) return `${(ms * 1000).toFixed(0)} µs`;
  if (ms < 1000) return `${ms.toFixed(ms < 10 ? 1 : 0)} ms`;
  return `${(ms / 1000).toFixed(2)} s`;
}

export function fmtUs(us: number | null | undefined): string {
  return us === null || us === undefined ? "" : fmtMs(us / 1000);
}

export function pct(part: number, whole: number): string {
  return whole ? `${((100 * part) / whole).toFixed(1)}%` : "";
}

const tone: Record<string, string> = {
  completed: "ok", SUCCEEDED: "ok", ACTIVE: "ok", closed: "ok", true: "ok", pass: "ok",
  acknowledged: "info", inProgress: "info", pending: "info", READY: "info", BLOCKED: "muted", compensating: "warn",
  held: "warn", partial: "warn", COMPENSATED: "warn", SUSPENDED: "warn", "half-open": "warn", OFFBOARDING: "warn",
  failed: "bad", rejected: "bad", FAILED: "bad", open: "bad", false: "bad", fail: "bad",
  cancelled: "muted", SKIPPED: "muted", GOLD: "ok", SILVER: "info", BRONZE: "muted",
};

export function toneOf(s: string): string {
  return tone[s] ?? "muted";
}

export const orderStates = ["acknowledged", "inProgress", "pending", "held", "completed", "partial", "failed", "cancelled", "rejected"];
export const taskStates = ["BLOCKED", "READY", "SUCCEEDED", "FAILED", "COMPENSATED", "SKIPPED"];
export const domains = ["sba", "ims", "netconf", "esim", "bss", "access", "exposure"];

// composeKey is the sessionStorage key the Template Lab uses to hand an order to the composer.
export const composeKey = "dxps.compose";

export function prio(p: number | string): string {
  return `P${p}`;
}

export function json(v: unknown): string {
  return JSON.stringify(v, null, 2);
}

// parseJson parses text or returns an error message.
export function parseJson<T = unknown>(text: string): { value?: T; error?: string } {
  try {
    return { value: JSON.parse(text) as T };
  } catch (e) {
    return { error: e instanceof Error ? e.message : String(e) };
  }
}
