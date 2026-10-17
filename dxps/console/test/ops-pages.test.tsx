import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import Dashboard from "@/app/(main)/page";
import DeadLetters from "@/app/(main)/dlq/page";
import KafkaPage from "@/app/(main)/kafka/page";
import Network from "@/app/(main)/network/page";
import OrderPage from "@/app/(main)/orders/[tenant]/[id]/page";
import Orders from "@/app/(main)/orders/page";
import Tasks from "@/app/(main)/tasks/page";
import LoginPage from "@/app/login/page";
import { Shell } from "@/components/Shell";
import { dlq, nes, order, overview, tenants } from "./fixtures";
import { assigned, mockFetch, renderAs, reply } from "./helpers";
import { nav } from "./nav";

describe("shell and login", () => {
  it("shows the navigation allowed for the role and signs out", async () => {
    const calls = mockFetch({ "GET auth/me": { username: "olga", roles: ["operator"] }, "POST auth/logout": { ok: true } });
    nav.pathname = "/orders";
    render(<Shell>content</Shell>);
    expect(await screen.findByText("content")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Service orders" })).toHaveClass("active");
    expect(screen.getByRole("link", { name: "Order composer" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Template Lab" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Users" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(assigned()).toContain("/login"));
    expect(calls.find((c) => c.path === "auth/logout")?.headers["x-dxps-csrf"]).toBe("1");
  });

  it("shows an error when the session cannot be loaded", async () => {
    mockFetch({ "GET auth/me": reply(500, { error: "API down" }) });
    render(<Shell>x</Shell>);
    expect(await screen.findByText("API down")).toBeInTheDocument();
  });

  it("signs in and reports a failure", async () => {
    mockFetch({ "POST auth/login": reply(401, { error: "invalid credentials" }) });
    render(<LoginPage />);
    await userEvent.type(screen.getByLabelText("User name"), "demo");
    await userEvent.type(screen.getByLabelText("Password"), "wrong");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByText("invalid credentials")).toBeInTheDocument();
    mockFetch({ "POST auth/login": { username: "demo", roles: ["admin"] } });
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await waitFor(() => expect(assigned()).toContain("/"));
  });
});

describe("dashboard", () => {
  it("summarises health, volumes and recent orders", async () => {
    mockFetch({ "GET overview": overview });
    renderAs(<Dashboard />);
    expect(await screen.findByText("mosaic: down")).toBeInTheDocument();
    expect(screen.getByText("udr01 open")).toBeInTheDocument();
    expect(screen.getByText("Service orders").previousSibling).toHaveTextContent("11");
    expect(screen.getByRole("link", { name: "11111111" })).toHaveAttribute("href", "/orders/host-mno/11111111-aaaa");
    expect(screen.getByText(/P1: 1/)).toBeInTheDocument();
    expect(screen.getByText("7 delivered · 0 failed")).toBeInTheDocument();
  });

  it("copes with missing statistics", async () => {
    mockFetch({ "GET overview": { at: "", errors: {} } });
    renderAs(<Dashboard />);
    expect(await screen.findByText("Orchestrator statistics unavailable")).toBeInTheDocument();
  });
});

describe("orders", () => {
  it("filters, searches and pages", async () => {
    const calls = mockFetch({
      "GET tenants": tenants,
      "GET orders*": { rows: [{ ...order, tasks: 2, error: "x", errorCode: "DXPS-1" }], total: 120 },
    });
    nav.search = new URLSearchParams("tenant=host-mno");
    renderAs(<Orders />);
    expect(await screen.findByText("CreateSubscriber5G")).toBeInTheDocument();
    expect(calls.at(-1)!.path).toBe("orders?tenant=host-mno&limit=50&offset=0");
    await userEvent.selectOptions(screen.getByLabelText("State"), "failed");
    await userEvent.type(screen.getByLabelText("Search"), "E1");
    await userEvent.click(screen.getByRole("button", { name: "Search" }));
    await waitFor(() => expect(calls.at(-1)!.path).toBe("orders?tenant=host-mno&state=failed&q=E1&limit=50&offset=0"));
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() => expect(calls.at(-1)!.path).toContain("offset=50"));
    await userEvent.click(screen.getByText("CreateSubscriber5G"));
    expect(nav.push).toHaveBeenCalledWith("/orders/host-mno/11111111-aaaa");
  });

  it("shows an order, its tasks and cancels it", async () => {
    const calls = mockFetch({ "GET orders/host-mno/o1": order, "POST orders/host-mno/o1/cancel": { status: 202, response: {} } });
    nav.params = { tenant: "host-mno", id: "o1" };
    renderAs(<OrderPage />, ["operator"]);
    expect(await screen.findByText("udr.auth.put on udr01")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "am" }));
    expect(await screen.findByText("boom")).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("Cancellation reason"), "customer");
    await userEvent.click(screen.getByRole("button", { name: "Cancel order" }));
    expect(await screen.findByText("Gateway answered HTTP 202")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "POST")!.body).toEqual({ reason: "customer" });
    for (const tab of ["Business commands", "Attempts", "TMF641 request", "Audit"]) {
      await userEvent.click(screen.getByRole("tab", { name: tab }));
    }
    expect(screen.getByText("order.created")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("tab", { name: "Attempts" }));
    expect(screen.getByText("3.0 ms")).toBeInTheDocument();
  });

  it("hides cancel for viewers", async () => {
    mockFetch({ "GET orders/host-mno/o1": { ...order, state: "completed" } });
    nav.params = { tenant: "host-mno", id: "o1" };
    renderAs(<OrderPage />, ["viewer"]);
    await screen.findByText("Summary");
    expect(screen.queryByRole("button", { name: "Cancel order" })).toBeNull();
  });

  it("lists NE tasks with filters", async () => {
    const calls = mockFetch({
      "GET tenants": tenants,
      "GET nes": nes,
      "GET tasks*": [{ tenant: "host-mno", id: "t", orderId: "o1234567", externalId: "", ne: "udr01", domain: "sba", operation: "udr.x", state: "FAILED", attempt: 1, priority: 1, compensation: true, lastError: "e", updatedAt: "" }],
    });
    renderAs(<Tasks />);
    expect(await screen.findByText("↺ udr.x")).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText("Domain"), "sba");
    await userEvent.selectOptions(screen.getByLabelText("State"), "FAILED");
    await waitFor(() => expect(calls.at(-1)!.path).toBe("tasks?state=FAILED&domain=sba&limit=200"));
  });
});

describe("messaging and network", () => {
  it("inspects and redrives dead letters", async () => {
    const calls = mockFetch({ "GET dlq*": dlq, "POST dlq/redrive": { replayed: 1, skipped: 1 } });
    vi.spyOn(window, "confirm").mockReturnValue(true);
    renderAs(<DeadLetters />, ["operator"]);
    await userEvent.click(await screen.findByText("udr.am.patch"));
    expect(screen.getByRole("link", { name: /Open order o1/ })).toHaveAttribute("href", "/orders/host-mno/o1");
    await userEvent.click(screen.getByText("decode"));
    expect(screen.getByText("raw")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Redrive (1)" }));
    expect(await screen.findByText("Replayed 1 record(s), skipped 1")).toBeInTheDocument();
    expect(calls.some((c) => c.path === "dlq/redrive")).toBe(true);
  });

  it("shows NEs and simulators and injects faults", async () => {
    const calls = mockFetch({
      "GET nes": nes,
      "GET sims": {
        sims: [{ name: "udr", port: 9102, calls: 5, byStatus: { "200": 5 }, p50us: 1, p99us: 2, faults: 0, latencyMs: 2, errorRate: 0, objects: 3 }],
        exchanges: [{ sim: "udr", at: "", tenant: "host-mno", method: "PUT", path: "/x", proto: "h2", status: 201, request: { q: 1 }, response: { r: 2 }, latUs: 10 }],
      },
      "POST sims/fault": {},
    });
    renderAs(<Network />, ["operator"]);
    expect(await screen.findByText("Select a simulator on the left.")).toBeInTheDocument();
    await userEvent.click(screen.getByText("9102"));
    fireEvent.change(screen.getByLabelText(/Error rate/), { target: { value: "0.5" } });
    await userEvent.click(screen.getByRole("button", { name: "Apply" }));
    expect(await screen.findByText("udr: latency 2 ms, error rate 50%")).toBeInTheDocument();
    expect(calls.find((c) => c.path === "sims/fault")!.body).toEqual({ sim: "udr", latencyMs: 2, errorRate: 0.5 });
    await userEvent.click(screen.getByRole("button", { name: "Reset to healthy" }));
    await waitFor(() => expect(calls.filter((c) => c.path === "sims/fault").at(-1)!.body).toEqual({ sim: "udr", latencyMs: 1, errorRate: 0 }));
    await userEvent.click(screen.getByText("PUT /x"));
    expect(screen.getByText(/"r": 2/)).toBeInTheDocument();
  });

  it("groups Kafka topics by priority lane", async () => {
    mockFetch({
      "GET kafka": {
        topics: [{ topic: "dxps.bc.p0", end: 3 }, { topic: "dxps.bc.p1", end: 1200 }, { topic: "dxps.dlq", end: 2 }],
        lags: [{ group: "a", lag: 0, state: "Empty" }, { group: "b", lag: 2000, state: "Rebalancing" }],
      },
    });
    renderAs(<KafkaPage />);
    expect(await screen.findAllByText("dxps.bc")).toHaveLength(1);
    expect(screen.getAllByText("1,200").length).toBeGreaterThan(0);
    expect(screen.getAllByText("2,000").length).toBeGreaterThan(0);
  });
});
