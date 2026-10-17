import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import AccountPage from "@/app/(main)/account/page";
import Audit from "@/app/(main)/audit/page";
import Hubs from "@/app/(main)/hubs/page";
import LoadPage from "@/app/(main)/loadgen/page";
import Reports from "@/app/(main)/reports/page";
import Settings from "@/app/(main)/settings/page";
import Tests from "@/app/(main)/tests/page";
import UsersPage from "@/app/(main)/users/page";
import { Dag, Pager, Spark, StackBar } from "@/components/ui";
import { overview } from "./fixtures";
import { mockFetch, renderAs } from "./helpers";

describe("insight", () => {
  it("shows reports for a period", async () => {
    const calls = mockFetch({
      "GET reports*": {
        days: 30,
        byDay: [{ day: "2026-10-07", tenant: "a", state: "completed", n: 9 }, { day: "2026-10-07", tenant: "a", state: "failed", n: 1 }],
        bySpec: [{ spec: "AddVoNR", items: 10, completed: 9, failed: 1, p50ms: 5, p95ms: 50 }],
        byNe: [{ ne: "udr01", calls: 10, p50us: 1000, p99us: 2000, errors: 1 }],
        errors: [{ code: "DXPS-3000", tenant: "a", n: 1, sample: "failed" }],
        tasksByDomain: [],
      },
    });
    renderAs(<Reports />);
    expect(await screen.findByText("DXPS-3000")).toBeInTheDocument();
    expect(screen.getAllByText("90.0%").length).toBeGreaterThan(0);
    await userEvent.selectOptions(screen.getByLabelText("Period"), "7");
    await waitFor(() => expect(calls.at(-1)!.path).toBe("reports?days=7"));
  });

  it("starts load and probes", async () => {
    const calls = mockFetch({
      "GET loadgen": {
        demo: { running: false, started: "", target: 10, sent: 10, byStatus: { "201": 9, "400": 1, "503": 1 }, byCode: { "DXPS-1": 1 }, bySpec: { A: 10 }, p50ms: 3, p99ms: 9, lastError: "oops", probing: false },
        probes: [{ id: "p1", category: "auth", name: "no token", expect: "401", got: "401", pass: true }],
        history: [{ t: 1, orders: 1, tasks: 2, p99: 3, lag: 0 }, { t: 2, orders: 5, tasks: 9, p99: 4, lag: 1 }],
      },
      "POST loadgen": {},
      "POST probes": {},
    });
    renderAs(<LoadPage />, ["operator"]);
    expect(await screen.findByText("1 / 1 passed")).toBeInTheDocument();
    expect(screen.getByText("Last error: oops")).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText(/Chaos/), { target: { value: "0.2" } });
    await userEvent.clear(screen.getByLabelText("Orders (1-20000)"));
    await userEvent.type(screen.getByLabelText("Orders (1-20000)"), "100");
    await userEvent.click(screen.getByRole("button", { name: "Start" }));
    expect(await screen.findByText("Started 100 orders at 50/s")).toBeInTheDocument();
    expect(calls.find((c) => c.path === "loadgen" && c.method === "POST")!.body).toEqual({ count: 100, rate: 50, chaos: 0.2 });
    await userEvent.click(screen.getByRole("button", { name: "Run probes" }));
    expect(await screen.findByText("Security probes started")).toBeInTheDocument();
  });

  it("shows test results or explains how to produce them", async () => {
    mockFetch({ "GET tests": { available: false } });
    const { unmount } = renderAs(<Tests />);
    expect(await screen.findByText(/No test report yet/)).toBeInTheDocument();
    unmount();
    mockFetch({
      "GET tests": {
        generatedAt: "2026-10-07T10:00:00Z", elapsedSec: 12, coverage: 91.5, totals: { tests: 2, passed: 1, failed: 1, skipped: 0 },
        packages: [{ name: "dxps/x", tests: 2, passed: 1, failed: 1, skipped: 0, coverage: 91.5 }],
        results: [{ package: "dxps/x", test: "TestA", action: "fail", elapsed: 0.1 }, { package: "dxps/x", test: "TestB", action: "pass", elapsed: 0.2 }],
      },
    });
    renderAs(<Tests />);
    expect(await screen.findByText("TestA")).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText("Show"), "pass");
    expect(screen.getByText("TestB")).toBeInTheDocument();
  });

  it("lists webhooks", async () => {
    mockFetch({ "GET hubs": [{ tenant: "a", id: "h", callback: "https://cb", secretRef: "enc:v1:x", status: "ACTIVE", createdAt: "" }], "GET overview": overview });
    renderAs(<Hubs />);
    expect(await screen.findByText("https://cb")).toBeInTheDocument();
    expect(screen.getByText("encrypted")).toBeInTheDocument();
  });

  it("searches both audit trails", async () => {
    const calls = mockFetch({
      "GET tenants": [],
      "GET audit?source=console*": [{ at: "", actor: "alice", action: "draft.save", object: "d1", detail: { a: 1 }, source: "console" }],
      "GET audit*": [
        { tenant: "a", id: "1", at: "", actor: "api", actingForTenant: true, action: "order.created", objectType: "service_order", objectId: "o12345678", detail: { x: 1 } },
        { tenant: "a", id: "2", at: "", actor: "console:alice", actingForTenant: false, action: "tenant.update", objectType: "tenant", objectId: "a" },
      ],
    });
    renderAs(<Audit />);
    expect(await screen.findByRole("link", { name: "order o1234567" })).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("Action"), "order");
    await userEvent.click(screen.getByRole("button", { name: "Search" }));
    await waitFor(() => expect(calls.at(-1)!.path).toBe("audit?action=order&limit=300"));
    await userEvent.click(screen.getByRole("tab", { name: "Console" }));
    expect(await screen.findByText("draft.save")).toBeInTheDocument();
  });
});

describe("administration", () => {
  it("manages users", async () => {
    const calls = mockFetch({
      "GET users": [{ name: "alice", roles: ["admin"], disabled: false, created: "" }, { name: "bob", roles: ["viewer"], disabled: false, created: "" }],
      "POST users": { ok: true },
      "PUT users/bob": { ok: true },
      "DELETE users/bob": { ok: true },
    });
    vi.spyOn(window, "confirm").mockReturnValue(true);
    vi.spyOn(window, "prompt").mockReturnValue("a-new-long-password");
    renderAs(<UsersPage />, ["admin"], "alice");
    await screen.findByText("bob");
    await userEvent.click(screen.getAllByLabelText("operator")[1]);
    await waitFor(() => expect(calls.find((c) => c.method === "PUT")!.body).toEqual({ roles: ["viewer", "operator"], disabled: false }));
    await userEvent.click(screen.getByLabelText("enable bob"));
    await userEvent.click(screen.getAllByRole("button", { name: "reset password" })[1]);
    await waitFor(() => expect(calls.filter((c) => c.method === "PUT").at(-1)!.body).toMatchObject({ password: "a-new-long-password" }));
    expect(screen.getAllByRole("button", { name: "delete" })).toHaveLength(1);
    await userEvent.click(screen.getByRole("button", { name: "delete" }));
    expect(await screen.findByText("User bob deleted")).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("User name"), "carol");
    await userEvent.type(screen.getByLabelText("Initial password (12+ characters)"), "carol-password-1");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    expect(await screen.findByText("User carol created")).toBeInTheDocument();
  });

  it("changes the own password", async () => {
    const calls = mockFetch({ "POST auth/password": { ok: true } });
    renderAs(<AccountPage />, ["viewer"], "bob");
    expect(screen.getByText("bob")).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("Current password"), "old-password-1");
    await userEvent.type(screen.getByLabelText("New password (at least 12 characters)"), "new-password-12");
    await userEvent.type(screen.getByLabelText("Repeat new password"), "different-pass1");
    await userEvent.click(screen.getByRole("button", { name: "Change password" }));
    expect(await screen.findByText("the new passwords differ")).toBeInTheDocument();
    await userEvent.clear(screen.getByLabelText("Repeat new password"));
    await userEvent.type(screen.getByLabelText("Repeat new password"), "new-password-12");
    await userEvent.click(screen.getByRole("button", { name: "Change password" }));
    expect(await screen.findByText("Password changed")).toBeInTheDocument();
    expect(calls[0].body).toEqual({ old: "old-password-1", new: "new-password-12" });
  });

  it("shows settings", async () => {
    mockFetch({
      "GET settings": { api: "https://a", catalogDir: "/c", gateway: "https://g", kafka: ["k:1"], mosaic: "http://m", results: "/r", runtime: "/rt", sessionHours: 8, stateDir: "/s" },
      "GET overview": overview,
    });
    renderAs(<Settings />);
    expect(await screen.findByText("https://g/tmf-api/serviceOrdering/v5")).toBeInTheDocument();
    expect(screen.getByText("8 hours")).toBeInTheDocument();
  });
});

describe("components", () => {
  it("draws graphs, bars and pagers", () => {
    const onSelect = vi.fn();
    render(
      <>
        <Dag nodes={[{ id: "a", title: "a very long task title that is clipped", sub: "a sub line that is also far too long to fit", deps: [] }, { id: "b", title: "b", deps: ["a", "missing"] }, { id: "c", title: "c", deps: ["c"] }]} onSelect={onSelect} />
        <Dag nodes={[]} />
        <StackBar parts={[{ label: "ok", n: 2, tone: "ok" }, { label: "zero", n: 0, tone: "bad" }]} />
        <Spark values={[1]} label="few" />
        <Pager offset={0} limit={10} total={0} onChange={vi.fn()} />
      </>,
    );
    fireEvent.click(screen.getByRole("button", { name: "b" }));
    expect(onSelect).toHaveBeenCalledWith("b");
    expect(screen.getByText("No tasks")).toBeInTheDocument();
    expect(screen.getByRole("img", { name: "ok 2, zero 0" })).toBeInTheDocument();
    expect(screen.getByText("few: not enough samples yet")).toBeInTheDocument();
    expect(screen.getByText("0 results")).toBeInTheDocument();
  });
});
