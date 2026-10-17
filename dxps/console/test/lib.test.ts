// @vitest-environment node
import { describe, expect, it, vi } from "vitest";
import { proxy, readCookie, upstreamUrl, type ProxyConfig } from "@/lib/proxy";
import { allows, fmtMs, fmtNum, fmtTime, fmtUs, parseJson, pct, prio, toneOf } from "@/lib/format";
import { qs } from "@/lib/api";

vi.mock("undici", () => ({
  Agent: class {
    constructor(public o: unknown) {}
  },
  fetch: vi.fn(async () => new Response("up")),
}));

function cfg(handler: (url: string, init: RequestInit) => Response | Promise<Response>, secure = true) {
  const calls: [string, RequestInit][] = [];
  const c: ProxyConfig = {
    apiUrl: "https://api:8446/",
    secureCookie: secure,
    fetch: async (url, init) => {
      calls.push([url, init]);
      return handler(url, init);
    },
  };
  return { c, calls };
}

const req = (method: string, path: string, init: RequestInit = {}) => new Request(`https://ui:8092/bff/${path}`, { method, ...init });

describe("proxy", () => {
  it("forwards GET with the session cookie as bearer token and filters headers", async () => {
    const { c, calls } = cfg(() => new Response('{"a":1}', { headers: { "content-type": "application/json", "x-internal": "no" } }));
    const res = await proxy(req("GET", "orders?limit=5", { headers: { cookie: "x=1; dxps_console=tok%3D", accept: "application/json", "x-forwarded-for": "1.2.3.4" } }), ["orders"], c);
    expect(await res.json()).toEqual({ a: 1 });
    expect(res.headers.get("x-internal")).toBeNull();
    const [url, init] = calls[0];
    expect(url).toBe("https://api:8446/api/v1/orders?limit=5");
    const h = init.headers as Headers;
    expect(h.get("authorization")).toBe("Bearer tok=");
    expect(h.get("x-forwarded-for")).toBeNull();
  });

  it("requires the CSRF header on mutations and rejects bad paths", async () => {
    const { c, calls } = cfg(() => new Response("{}"));
    expect((await proxy(req("POST", "x", { body: "{}" }), ["x"], c)).status).toBe(403);
    expect((await proxy(req("GET", "x"), ["a", ".."], c)).status).toBe(400);
    expect((await proxy(req("GET", "x"), ["a/b"], c)).status).toBe(400);
    expect(calls).toHaveLength(0);
  });

  it("keeps the login token in an HttpOnly cookie", async () => {
    const exp = new Date(Date.now() + 3600_000).toISOString();
    const { c, calls } = cfg(() => new Response(JSON.stringify({ token: "T", username: "u", roles: ["admin"], expires_at: exp })));
    const res = await proxy(req("POST", "auth/login", { body: '{"username":"u"}', headers: { "x-dxps-csrf": "1" } }), ["auth", "login"], c);
    expect(await res.json()).toEqual({ username: "u", roles: ["admin"], expires_at: exp });
    expect(res.headers.get("set-cookie")).toMatch(/^dxps_console=T; Path=\/; HttpOnly; SameSite=Strict; Max-Age=35\d\d; Secure$/);
    expect(new TextDecoder().decode(calls[0][1].body as ArrayBuffer)).toBe('{"username":"u"}');
  });

  it("clears the cookie on logout and 401, and reports an unreachable API", async () => {
    const { c } = cfg(() => new Response(null, { status: 401 }), false);
    const res = await proxy(req("GET", "auth/me"), ["auth", "me"], c);
    expect(res.headers.get("set-cookie")).toBe("dxps_console=; Path=/; HttpOnly; SameSite=Strict; Max-Age=0");
    const ok = cfg(() => new Response("{}"));
    expect((await proxy(req("POST", "auth/logout", { headers: { "x-dxps-csrf": "1" } }), ["auth", "logout"], ok.c)).headers.get("set-cookie")).toContain("Max-Age=0");
    const down = cfg(() => Promise.reject(new Error("refused")));
    const r = await proxy(req("GET", "overview"), ["overview"], down.c);
    expect(r.status).toBe(502);
  });

  it("parses cookies and builds upstream URLs", () => {
    expect(readCookie("a=1; b=2", "b")).toBe("2");
    expect(readCookie(null, "b")).toBe("");
    const { c } = cfg(() => new Response());
    expect(upstreamUrl(c, ["orders", "host-mno", "x y"], "?q=1")).toBe("https://api:8446/api/v1/orders/host-mno/x%20y?q=1");
    expect(upstreamUrl(c, ["."], "")).toBeNull();
  });

  it("builds the upstream configuration from the environment", async () => {
    const { upstream } = await import("@/lib/upstream");
    const u = upstream({ DXPS_CONSOLE_API_URL: "https://x:1", DXPS_CONSOLE_INSECURE_COOKIE: "1" });
    expect(u.apiUrl).toBe("https://x:1");
    expect(u.secureCookie).toBe(false);
    expect(await (await u.fetch("https://x:1/a", {})).text()).toBe("up");
    expect(upstream({}).apiUrl).toBe("https://localhost:8446");
  });
});

describe("format", () => {
  it("applies the role hierarchy", () => {
    expect(allows(["admin"], "viewer")).toBe(true);
    expect(allows(["operator"], "configurator")).toBe(false);
    expect(allows(["configurator"], "operator")).toBe(true);
    expect(allows(undefined, "viewer")).toBe(false);
  });

  it("formats numbers, times and durations", () => {
    expect(fmtTime("2026-10-07T12:00:00.123Z")).toBe("2026-10-07 12:00:00Z");
    expect(fmtTime("0001-01-01T00:00:00Z")).toBe("");
    expect(fmtTime(undefined)).toBe("");
    expect(fmtNum(12345)).toBe("12,345");
    expect(fmtNum(undefined)).toBe("");
    expect(fmtMs(0.5)).toBe("500 µs");
    expect(fmtMs(4.25)).toBe("4.3 ms");
    expect(fmtMs(250)).toBe("250 ms");
    expect(fmtMs(2500)).toBe("2.50 s");
    expect(fmtMs(null)).toBe("");
    expect(fmtUs(4500)).toBe("4.5 ms");
    expect(fmtUs(undefined)).toBe("");
    expect(pct(1, 4)).toBe("25.0%");
    expect(pct(1, 0)).toBe("");
    expect(prio(1)).toBe("P1");
  });

  it("maps states to tones and parses JSON", () => {
    expect(toneOf("completed")).toBe("ok");
    expect(toneOf("FAILED")).toBe("bad");
    expect(toneOf("held")).toBe("warn");
    expect(toneOf("whatever")).toBe("muted");
    expect(parseJson('{"a":1}').value).toEqual({ a: 1 });
    expect(parseJson("{").error).toBeTruthy();
  });

  it("builds query strings from non-empty values", () => {
    expect(qs({ a: "x", b: "", c: undefined, d: 0, e: false, f: null })).toBe("?a=x&d=0");
    expect(qs({})).toBe("");
  });
});
