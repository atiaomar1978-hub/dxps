// Backend-for-frontend proxy: the browser talks to /bff/* on the console origin; the session token lives in an
// HttpOnly cookie and is forwarded to the console API as a bearer token. Mutating requests need the x-dxps-csrf
// header, which a cross-site page cannot send without a CORS preflight the console never grants.

export const COOKIE = "dxps_console";

export type Fetch = (url: string, init: RequestInit & { duplex?: string }) => Promise<Response>;

export interface ProxyConfig {
  apiUrl: string;
  fetch: Fetch;
  secureCookie: boolean;
}

const forwardRequest = ["content-type", "accept"];
const forwardResponse = ["content-type", "cache-control"];

export function readCookie(header: string | null, name: string): string {
  for (const part of (header ?? "").split(";")) {
    const [k, ...v] = part.trim().split("=");
    if (k === name) return decodeURIComponent(v.join("="));
  }
  return "";
}

function cookie(cfg: ProxyConfig, value: string, maxAge: number): string {
  const attrs = [`${COOKIE}=${encodeURIComponent(value)}`, "Path=/", "HttpOnly", "SameSite=Strict", `Max-Age=${maxAge}`];
  if (cfg.secureCookie) attrs.push("Secure");
  return attrs.join("; ");
}

function json(status: number, body: unknown, extra: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json", ...extra } });
}

export function upstreamUrl(cfg: ProxyConfig, path: string[], search: string): string | null {
  if (path.some((p) => p === "" || p === "." || p === ".." || p.includes("/"))) return null;
  return `${cfg.apiUrl.replace(/\/$/, "")}/api/v1/${path.map(encodeURIComponent).join("/")}${search}`;
}

export async function proxy(req: Request, path: string[], cfg: ProxyConfig): Promise<Response> {
  const method = req.method.toUpperCase();
  if (method !== "GET" && method !== "HEAD" && req.headers.get("x-dxps-csrf") !== "1") {
    return json(403, { error: "missing CSRF header" });
  }
  const url = upstreamUrl(cfg, path, new URL(req.url).search);
  if (!url) return json(400, { error: "invalid path" });

  const headers = new Headers();
  for (const h of forwardRequest) {
    const v = req.headers.get(h);
    if (v) headers.set(h, v);
  }
  const token = readCookie(req.headers.get("cookie"), COOKIE);
  if (token) headers.set("authorization", `Bearer ${token}`);

  const body = method === "GET" || method === "HEAD" ? undefined : await req.arrayBuffer();
  let res: Response;
  try {
    res = await cfg.fetch(url, { method, headers, body, redirect: "manual", signal: req.signal });
  } catch {
    return json(502, { error: "console API unavailable" });
  }

  const key = path.join("/");
  if (key === "auth/login" && res.ok) {
    const out = (await res.json()) as { token: string; expires_at: string; username: string; roles: string[] };
    const maxAge = Math.max(0, Math.floor((Date.parse(out.expires_at) - Date.now()) / 1000));
    return json(200, { username: out.username, roles: out.roles, expires_at: out.expires_at }, { "set-cookie": cookie(cfg, out.token, maxAge) });
  }

  const out = new Headers();
  for (const h of forwardResponse) {
    const v = res.headers.get(h);
    if (v) out.set(h, v);
  }
  if (key === "auth/logout" || res.status === 401) out.append("set-cookie", cookie(cfg, "", 0));
  return new Response(res.body, { status: res.status, headers: out });
}
