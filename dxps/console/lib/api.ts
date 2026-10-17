// Browser client for the BFF (/bff/* is proxied to the console API /api/v1/*).

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

export interface Options {
  method?: string;
  body?: unknown;
}

export const base = "/bff/";

export function onUnauthorized() {
  if (!window.location.pathname.startsWith("/login")) window.location.assign("/login");
}

export async function api<T = unknown>(path: string, o: Options = {}): Promise<T> {
  const method = o.method ?? (o.body === undefined ? "GET" : "POST");
  const headers: Record<string, string> = { accept: "application/json" };
  if (method !== "GET") headers["x-dxps-csrf"] = "1";
  let body: BodyInit | undefined;
  if (o.body !== undefined) {
    headers["content-type"] = "application/json";
    body = JSON.stringify(o.body);
  }
  const res = await fetch(base + path.replace(/^\//, ""), { method, headers, body, credentials: "same-origin" });
  const text = await res.text();
  let data: unknown = undefined;
  try {
    data = text ? JSON.parse(text) : undefined;
  } catch {
    data = text;
  }
  if (!res.ok) {
    if (res.status === 401 && !path.startsWith("auth/login")) onUnauthorized();
    const e = (data ?? {}) as { error?: string };
    throw new ApiError(res.status, e.error ?? `HTTP ${res.status}`);
  }
  return data as T;
}

// qs builds a query string from the non-empty values.
export function qs(params: Record<string, string | number | boolean | undefined | null>): string {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== "" && v !== false) p.set(k, String(v));
  }
  const s = p.toString();
  return s ? `?${s}` : "";
}

export function errorText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
