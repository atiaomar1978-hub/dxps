import { fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import SpecPage from "@/app/(main)/catalog/[tenant]/[spec]/[version]/page";
import Catalog from "@/app/(main)/catalog/page";
import Compose from "@/app/(main)/compose/page";
import Inventory from "@/app/(main)/inventory/page";
import Lab from "@/app/(main)/lab/page";
import DraftEditor from "@/app/(main)/studio/[id]/page";
import Studio from "@/app/(main)/studio/page";
import Tenants from "@/app/(main)/tenants/page";
import { composeKey } from "@/lib/format";
import { catalog, lab, nes, spec, tenants } from "./fixtures";
import { mockFetch, renderAs } from "./helpers";
import { nav } from "./nav";

const draft = {
  id: "abcdef0123456789",
  name: "My draft",
  author: "alice",
  created: "",
  updated: "",
  files: { "specs/mobile.yaml": "a: 1\n", "templates/ims/new.json.tmpl": "{}" },
};

describe("catalog", () => {
  it("lists and filters specs", async () => {
    mockFetch({ "GET catalog": catalog });
    renderAs(<Catalog />);
    expect(await screen.findByText("CreateL3VPN")).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText("Domain"), "ims");
    expect(screen.queryByText("CreateL3VPN")).toBeNull();
    await userEvent.selectOptions(screen.getByLabelText("Owner"), "mvno-alpha");
    expect(screen.getAllByText("AddVoNR")).toHaveLength(1);
    await userEvent.type(screen.getByLabelText("Search"), "zzz");
    expect(screen.getByText("Nothing to show")).toBeInTheDocument();
  });

  it("shows a spec and starts drafts from it", async () => {
    const calls = mockFetch({ "GET catalog/spec*": spec, "GET tenants": tenants, "POST drafts": draft });
    nav.params = { tenant: "GLOBAL", spec: "AddVoNR", version: "1.1.0" };
    renderAs(<SpecPage />, ["configurator"]);
    expect(await screen.findByText("Task impi")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "impu" }));
    expect(screen.getByText("has(params.cfu)")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("tab", { name: "Parameters" }));
    expect(screen.getByText(/pattern \^imsi/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("tab", { name: "Templates" }));
    expect(screen.getByText('{"impi": "{{.Params.supi}}"}')).toBeInTheDocument();
    await userEvent.click(screen.getByRole("tab", { name: "YAML source" }));
    expect(screen.getByText(/spec: AddVoNR/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Try in Template Lab" })).toHaveAttribute("href", "/lab?spec=AddVoNR&version=1.1.0");

    await userEvent.selectOptions(screen.getByLabelText("For tenant"), "mvno-alpha");
    await userEvent.click(screen.getByRole("button", { name: "Create override draft" }));
    await waitFor(() => expect(nav.push).toHaveBeenCalledWith(`/studio/${draft.id}`));
    expect(calls.find((c) => c.method === "POST")!.body).toEqual({
      name: "mvno-alpha override of AddVoNR",
      override: { tenant: "GLOBAL", spec: "AddVoNR", version: "1.1.0", forTenant: "mvno-alpha" },
    });
    await userEvent.click(screen.getByRole("button", { name: "Edit in studio" }));
    await waitFor(() => expect(calls.filter((c) => c.method === "POST").at(-1)!.body).toEqual({ name: "Edit AddVoNR 1.1.0", files: { "specs/mobile.yaml": spec.source } }));
  });
});

describe("studio", () => {
  it("creates a draft from catalog files and a skeleton", async () => {
    const calls = mockFetch({ "GET drafts": [{ ...draft, files: Object.keys(draft.files) }], "GET catalog/files": { "specs/mobile.yaml": "x" }, "POST drafts": draft });
    renderAs(<Studio />, ["configurator"]);
    expect(await screen.findByRole("link", { name: "My draft" })).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("Name"), "New");
    await userEvent.selectOptions(screen.getByRole("listbox"), "specs/mobile.yaml");
    await userEvent.click(screen.getByRole("checkbox"));
    await userEvent.click(screen.getByRole("button", { name: "Create draft" }));
    await waitFor(() => expect(nav.push).toHaveBeenCalledWith(`/studio/${draft.id}`));
    const body = calls.find((c) => c.method === "POST")!.body as { files: Record<string, string> };
    expect(Object.keys(body.files)).toEqual(["specs/mobile.yaml", "specs/new-service.yaml"]);
  });

  it("edits, validates, exports and deletes a draft", async () => {
    const calls = mockFetch({
      "GET drafts/abcdef0123456789": { draft, base: { "specs/mobile.yaml": "a: 0\n" } },
      "GET catalog/files": { "specs/mobile.yaml": "a: 0\n", "specs/other.yaml": "o: 1\n" },
      "PUT drafts/abcdef0123456789": draft,
      "POST drafts/abcdef0123456789/validate": { ok: true, specs: [], smoke: [{ tenant: "host-mno", spec: "S", version: "1", ok: false, problems: ["bad"] }], changed: [] },
      "POST drafts/abcdef0123456789/export": { name: "My draft", files: { "specs/mobile.yaml": "a: 2\n" }, deleted: ["specs/old.yaml"], target: "dxps/internal/catalog/data" },
      "DELETE drafts/abcdef0123456789": { ok: true },
    });
    vi.spyOn(window, "confirm").mockReturnValue(true);
    nav.params = { id: draft.id };
    renderAs(<DraftEditor />, ["configurator"]);
    const editor = await screen.findByLabelText("file editor");
    expect(editor).toHaveValue("a: 1\n");
    fireEvent.change(editor, { target: { value: "a: 2\n" } });
    expect(screen.getByText("unsaved")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Draft saved")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "PUT")!.body).toEqual({ files: { ...draft.files, "specs/mobile.yaml": "a: 2\n" }, deleted: [] });

    await userEvent.selectOptions(screen.getByLabelText("Catalog file"), "specs/other.yaml");
    await userEvent.click(screen.getByRole("button", { name: "Add to draft" }));
    expect(screen.getByLabelText("file editor")).toHaveValue("o: 1\n");
    await userEvent.type(screen.getByLabelText("New file path"), "templates/sba/z.json.tmpl");
    await userEvent.click(screen.getByRole("button", { name: "Create file" }));
    expect(screen.getByLabelText("file editor")).toHaveValue("");
    await userEvent.click(screen.getByText("specs/mobile.yaml"));
    await userEvent.click(screen.getByRole("button", { name: /Remove mobile.yaml/ }));
    expect(screen.getByTitle("removed from the catalog when exported")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Validate" }));
    expect(await screen.findByText("The catalog loads with this draft")).toBeInTheDocument();
    expect(screen.getByText("bad")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Export" }));
    expect(await screen.findByText("Delete: specs/old.yaml")).toBeInTheDocument();
    URL.createObjectURL = vi.fn(() => "blob:x");
    URL.revokeObjectURL = vi.fn();
    await userEvent.click(screen.getByRole("button", { name: "Download bundle" }));
    expect(URL.createObjectURL).toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(nav.push).toHaveBeenCalledWith("/studio"));
  });

  it("is read-only for viewers", async () => {
    mockFetch({ "GET drafts/abcdef0123456789": { draft, base: {} }, "GET catalog/files": {} });
    nav.params = { id: draft.id };
    renderAs(<DraftEditor />, ["viewer"]);
    expect(await screen.findByLabelText("file editor")).toHaveAttribute("readonly");
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  });
});

describe("template lab and composer", () => {
  it("runs a dry run and hands the order to the composer", async () => {
    const calls = mockFetch({ "GET tenants": tenants, "GET catalog": catalog, "GET drafts": [], "GET catalog/spec*": spec, "POST lab": lab });
    nav.search = new URLSearchParams("tenant=mvno-alpha&spec=AddVoNR");
    renderAs(<Lab />, ["configurator"]);
    await waitFor(() => expect(screen.getByLabelText("parameters")).toHaveValue(JSON.stringify(spec.spec.example, null, 2)));
    expect(screen.getByText("Version (mvno-alpha)")).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("parameters"), { target: { value: "{" } });
    expect(screen.getByRole("button", { name: "Run" })).toBeDisabled();
    fireEvent.change(screen.getByLabelText("parameters"), { target: { value: '{"supi":"imsi-1"}' } });
    fireEvent.change(screen.getByLabelText("results"), { target: { value: '{"impi":{"a":1}}' } });
    await userEvent.click(screen.getByRole("button", { name: "Run" }));
    expect(await screen.findByText("mvno-alpha override")).toBeInTheDocument();
    expect(calls.find((c) => c.path === "lab")!.body).toEqual({ tenant: "mvno-alpha", spec: "AddVoNR", version: "", draft: "", params: { supi: "imsi-1" }, results: { impi: { a: 1 } } });
    expect(screen.getByText(/"impi": "x"/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("tab", { name: "Compensation" }));
    expect(screen.getByText(/"del": 1/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("tab", { name: "Selection" }));
    expect(screen.getByText("imspg01 (owner host-mno, shared)")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "skip (skipped)" }));
    expect(screen.getByText("no NE")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("tab", { name: "Payload" }));
    expect(screen.getByText("bad template")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Send as a real order…" }));
    expect(nav.push).toHaveBeenCalledWith("/compose");
    expect(JSON.parse(sessionStorage.getItem(composeKey)!)).toEqual({ tenant: "mvno-alpha", items: [{ id: "1", spec: "AddVoNR", action: "add", params: { supi: "imsi-1" } }] });
  });

  it("submits an order through the gateway", async () => {
    sessionStorage.setItem(composeKey, JSON.stringify({ tenant: "mvno-alpha", items: [{ id: "1", spec: "AddVoNR", action: "add", params: { supi: "imsi-1" } }] }));
    const calls = mockFetch({
      "GET tenants": tenants,
      "GET catalog": catalog,
      "GET catalog/spec*": spec,
      "POST orders": { status: 201, id: "new-order-id", request: { r: 1 }, response: { state: "acknowledged" } },
    });
    renderAs(<Compose />, ["operator"]);
    expect(await screen.findByLabelText("item 1 parameters")).toHaveValue(JSON.stringify({ supi: "imsi-1" }, null, 2));
    expect(sessionStorage.getItem(composeKey)).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Add item" }));
    await userEvent.selectOptions(screen.getAllByLabelText("Service spec")[1], "CreateL3VPN");
    await userEvent.type(screen.getAllByLabelText("Depends on items")[1], "1");
    await userEvent.click(screen.getAllByRole("button", { name: "Use example" })[0]);
    await userEvent.type(screen.getByLabelText("External id"), "CRM-1");
    await userEvent.click(screen.getByRole("button", { name: "Submit order" }));
    expect(await screen.findByRole("link", { name: /Follow order new-orde/ })).toHaveAttribute("href", "/orders/mvno-alpha/new-order-id");
    const body = calls.find((c) => c.path === "orders")!.body as { items: { dependsOn: string[] }[]; externalId: string };
    expect(body.externalId).toBe("CRM-1");
    expect(body.items[1].dependsOn).toEqual(["1"]);
    await userEvent.click(screen.getAllByRole("button", { name: "Remove" })[1]);
    expect(screen.getAllByLabelText("Service spec")).toHaveLength(1);
  });
});

describe("inventory", () => {
  it("edits a tenant", async () => {
    const calls = mockFetch({ "GET tenants": tenants, "PUT tenants/host-mno": { ok: true } });
    renderAs(<Tenants />, ["configurator"]);
    await userEvent.click(await screen.findByText("Host MNO"));
    await userEvent.clear(screen.getByLabelText("Weight (1-64)"));
    await userEvent.type(screen.getByLabelText("Weight (1-64)"), "12");
    await userEvent.selectOptions(screen.getByLabelText("Status"), "SUSPENDED");
    expect(screen.getByText(/new orders are rejected/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(/Tenant host-mno updated/)).toBeInTheDocument();
    expect(calls.find((c) => c.method === "PUT")!.body).toEqual({ name: "Host MNO", slaTier: "GOLD", slaWeight: 12, quotaTps: 500, status: "SUSPENDED" });
  });

  it("edits NE limits and entitlements", async () => {
    const calls = mockFetch({ "GET nes": nes, "GET tenants": tenants, "PUT nes/host-mno/udr01": { ok: true }, "PUT nes/host-mno/udr01/access": { ok: true } });
    renderAs(<Inventory />, ["configurator"]);
    await userEvent.click(await screen.findByText("udr01"));
    await userEvent.clear(screen.getByLabelText("Max TPS"));
    await userEvent.type(screen.getByLabelText("Max TPS"), "900");
    await userEvent.click(screen.getByRole("button", { name: "Save limits" }));
    expect(await screen.findByText("udr01 limits saved")).toBeInTheDocument();
    expect(calls.find((c) => c.path === "nes/host-mno/udr01")!.body).toEqual({ maxTps: 900, maxConcurrency: 64, reservedPct: 20 });
    await userEvent.click(screen.getAllByText("udr.*").at(-1)!);
    expect(screen.getByRole("button", { name: "Update entitlement" })).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText("Tenant"), "mvno-gamma");
    await userEvent.type(screen.getByLabelText("Subscriber group (optional)"), "g1");
    await userEvent.click(screen.getByRole("button", { name: "Grant entitlement" }));
    expect(await screen.findByText("mvno-gamma entitlement on udr01 saved")).toBeInTheDocument();
    expect(calls.find((c) => c.path.endsWith("/access"))!.body).toEqual({ tenant: "mvno-gamma", quotaTps: 400, allowedOps: ["udr.*"], subscriberGroup: "g1" });
    await userEvent.click(screen.getByText("ocs-alpha"));
    expect(screen.getByText(/Only mvno-alpha uses this element/)).toBeInTheDocument();
  });
});
