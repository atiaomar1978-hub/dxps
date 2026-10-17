import { render } from "@testing-library/react";
import type { ReactElement } from "react";
import { vi } from "vitest";
import { SessionProvider } from "@/components/Shell";

export interface Call {
  method: string;
  path: string;
  body: unknown;
  headers: Record<string, string>;
}

interface Reply {
  __status: number;
  body?: unknown;
  headers?: Record<string, string>;
}

export const reply = (status: number, body?: unknown, headers?: Record<string, string>): Reply => ({ __status: status, body, headers });

type Route = unknown | ((c: Call) => unknown);

// mockFetch answers "METHOD path" (query included; a trailing * matches a prefix) and records the calls.
export function mockFetch(routes: Record<string, Route>): Call[] {
  const calls: Call[] = [];
  const fn = vi.fn(async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const path = String(url).replace(/^\/bff\//, "");
    const body = typeof init.body === "string" ? JSON.parse(init.body) : undefined;
    const call = { method, path, body, headers: (init.headers ?? {}) as Record<string, string> };
    calls.push(call);
    const key = Object.keys(routes).find((k) => {
      const [m, p] = k.split(" ");
      return m === method && (p === path || (p.endsWith("*") && path.startsWith(p.slice(0, -1))));
    });
    if (!key) return new Response(JSON.stringify({ error: `no mock for ${method} ${path}` }), { status: 404 });
    let r = routes[key];
    if (typeof r === "function") r = (r as (c: Call) => unknown)(call);
    if (r && typeof r === "object" && "__status" in r) {
      const x = r as Reply;
      return new Response(x.body === undefined ? null : JSON.stringify(x.body), { status: x.__status, headers: x.headers });
    }
    return new Response(JSON.stringify(r), { status: 200, headers: { "content-type": "application/json" } });
  });
  globalThis.fetch = fn as unknown as typeof fetch;
  return calls;
}

export function renderAs(ui: ReactElement, roles: string[] = ["admin"], username = "alice") {
  return render(<SessionProvider value={{ username, roles }}>{ui}</SessionProvider>);
}

export function assigned(): string[] {
  return (window.location.assign as unknown as ReturnType<typeof vi.fn>).mock.calls.map((c) => String(c[0]));
}
