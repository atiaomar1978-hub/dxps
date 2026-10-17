"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { createContext, useContext, type ReactNode } from "react";
import { api } from "@/lib/api";
import { allows, type Role } from "@/lib/format";
import { useApi } from "@/lib/hooks";
import { ErrorBox } from "./ui";

export interface Session {
  username: string;
  roles: string[];
}

const SessionContext = createContext<Session>({ username: "", roles: [] });

export const SessionProvider = SessionContext.Provider;

export function useSession(): Session {
  return useContext(SessionContext);
}

// Can renders children only for callers holding the role.
export function Can({ role, children }: { role: Role; children: ReactNode }) {
  return allows(useSession().roles, role) ? <>{children}</> : null;
}

export const nav: { group: string; items: { href: string; label: string; role: Role }[] }[] = [
  {
    group: "Operations",
    items: [
      { href: "/", label: "Dashboard", role: "viewer" },
      { href: "/orders", label: "Service orders", role: "viewer" },
      { href: "/tasks", label: "NE tasks", role: "viewer" },
      { href: "/dlq", label: "Dead letters", role: "viewer" },
      { href: "/network", label: "Network & simulators", role: "viewer" },
      { href: "/kafka", label: "Kafka", role: "viewer" },
    ],
  },
  {
    group: "Configuration",
    items: [
      { href: "/catalog", label: "Service catalog", role: "viewer" },
      { href: "/studio", label: "Catalog studio", role: "viewer" },
      { href: "/lab", label: "Template Lab", role: "configurator" },
      { href: "/compose", label: "Order composer", role: "operator" },
      { href: "/tenants", label: "Tenants", role: "viewer" },
      { href: "/inventory", label: "NE inventory", role: "viewer" },
    ],
  },
  {
    group: "Insight",
    items: [
      { href: "/reports", label: "Reports", role: "viewer" },
      { href: "/loadgen", label: "Load & probes", role: "viewer" },
      { href: "/tests", label: "Test results", role: "viewer" },
      { href: "/hubs", label: "Webhooks", role: "viewer" },
      { href: "/audit", label: "Audit trail", role: "viewer" },
    ],
  },
  {
    group: "Administration",
    items: [
      { href: "/users", label: "Users", role: "admin" },
      { href: "/settings", label: "Settings", role: "viewer" },
      { href: "/account", label: "My account", role: "viewer" },
    ],
  },
];

export function active(path: string, href: string): boolean {
  return href === "/" ? path === "/" : path === href || path.startsWith(href + "/");
}

export function Shell({ children }: { children: ReactNode }) {
  const me = useApi<Session>("auth/me");
  const path = usePathname() ?? "/";
  if (!me.data) {
    return <div className="center">{me.error ? <ErrorBox error={me.error} /> : <p className="muted">Loading…</p>}</div>;
  }
  const logout = async () => {
    await api("auth/logout", { method: "POST" }).catch(() => undefined);
    window.location.assign("/login");
  };
  return (
    <SessionContext.Provider value={me.data}>
      <div className="shell">
        <aside className="sidebar">
          <div className="brand">
            <span>
              Dx<b>PS</b>
            </span>
            <span className="brand-sub">Provisioning console</span>
          </div>
          <nav>
            {nav.map((g) => (
              <div key={g.group} className="nav-group">
                <div className="nav-title">{g.group}</div>
                {g.items
                  .filter((i) => allows(me.data!.roles, i.role))
                  .map((i) => (
                    <Link key={i.href} href={i.href} className={active(path, i.href) ? "active" : ""}>
                      {i.label}
                    </Link>
                  ))}
              </div>
            ))}
          </nav>
        </aside>
        <div className="main">
          <div className="topbar">
            <span className="muted">Real-time provisioning · TMF641 / TMF688</span>
            <span>
              {me.data.username} <span className="muted">({me.data.roles.join(", ")})</span>{" "}
              <button className="link" onClick={logout}>
                Sign out
              </button>
            </span>
          </div>
          <main>{children}</main>
        </div>
      </div>
    </SessionContext.Provider>
  );
}
