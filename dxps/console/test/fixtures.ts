import type { DLQRecord, LabResult, NE, OrderDetail, Overview, SpecDetail, SpecSummary, Tenant } from "@/lib/types";

export const tenants: Tenant[] = [
  { tenant: "host-mno", name: "Host MNO", tenantType: "MNO", slaTier: "GOLD", slaWeight: 8, quotaTps: 500, status: "ACTIVE", orders: 10, failed: 1, active: 0, lastOrder: "2026-10-07T10:00:00Z", ownNes: 10, entitledNes: 0 },
  { tenant: "mvno-alpha", name: "Alpha", tenantType: "MVNO", hostTenant: "host-mno", slaTier: "SILVER", slaWeight: 4, quotaTps: 200, status: "ACTIVE", orders: 5, failed: 0, active: 1, ownNes: 1, entitledNes: 3 },
  { tenant: "mvno-gamma", name: "Gamma", tenantType: "MVNO", slaTier: "BRONZE", slaWeight: 1, quotaTps: 10, status: "SUSPENDED", orders: 0, failed: 0, active: 0, ownNes: 0, entitledNes: 0 },
];

export const overview: Overview = {
  at: "2026-10-16T10:00:00Z",
  up: { gateway: true, orchestrator: true, adapter: false },
  db: {
    ordersByState: [
      { tenant: "host-mno", key: "completed", n: 9 },
      { tenant: "host-mno", key: "failed", n: 1 },
      { tenant: "mvno-alpha", key: "inProgress", n: 1 },
    ],
    tasksByDomain: [
      { tenant: "host-mno", key: "sba", sub: "SUCCEEDED", n: 20 },
      { tenant: "host-mno", key: "sba", sub: "FAILED", n: 2 },
    ],
    neLatency: [{ ne: "udr01", calls: 22, p50us: 3000, p99us: 9000, errors: 2 }],
    recent: [{ tenant: "host-mno", id: "11111111-aaaa", externalId: "E1", state: "completed", priority: 1, spec: "AddVoNR", at: "2026-10-07T10:00:00Z", durationMs: 40 }],
    outboxBacklog: 0,
    ordersLastMin: 3,
    tasksLastMin: 9,
    p99OrderMs: 120,
  },
  tenants,
  services: {
    orchestrator: {
      bcLanes: { Lanes: 2, InFlight: 1, MaxLaneDepth: 3 },
      bcScheduler: { queued: [0, 1, 0, 0], served: [1, 2, 3, 4], byTenant: {} },
      resultLanes: { Lanes: 1, InFlight: 0, MaxLaneDepth: 1 },
      resultScheduler: { queued: [0, 0, 0, 0], served: [0, 0, 0, 0], byTenant: {} },
    },
    adapter: [{ ne: "udr01", calls: 1, errors: 0, lastUs: 1, totalUs: 1, breaker: "open" }],
    eventhub: { delivered: 7, failed: 0 },
  },
  kafka: { topics: [{ topic: "dxps.bc.p1", end: 10 }], lags: [{ group: "g", lag: 5, state: "Stable" }] },
  errors: { mosaic: "down" },
};

export const order: OrderDetail = {
  tenant: "host-mno",
  id: "11111111-aaaa",
  externalId: "E1",
  state: "inProgress",
  priority: 1,
  txMode: "ATOMIC",
  channel: "API",
  category: "",
  specs: "CreateSubscriber5G",
  requestedAt: "2026-10-07T10:00:00Z",
  durationMs: 0,
  idempotencyKey: "idem-1",
  items: { serviceOrderItem: [] },
  businessCommands: [
    { id: "b1", itemId: "1", spec: "CreateSubscriber5G", specVersion: "3.2.0", specTenant: "GLOBAL", action: "add", state: "inProgress", priority: 1, txMode: "ATOMIC", entityKey: "supi:1", params: { supi: "imsi-1" }, dependsOn: [], createdAt: "", updatedAt: "" },
  ],
  tasks: [
    { id: "t1", itemIds: ["b1"], specTask: "auth", ne: "udr01", neTenant: "host-mno", domain: "sba", operation: "udr.auth.put", state: "SUCCEEDED", attempt: 0, seq: 1, compensation: false, afterPivot: false, request: { a: 1 }, response: { ok: true }, neStatus: 201, createdAt: "", dependsOn: [], idempotencyKey: "k1" },
    { id: "t2", itemIds: ["b1"], specTask: "am", ne: "udr01", neTenant: "host-mno", domain: "sba", operation: "udr.am.put", state: "FAILED", attempt: 2, seq: 2, compensation: false, afterPivot: false, lastError: "boom", createdAt: "", dependsOn: ["t1"], idempotencyKey: "k2" },
  ],
  attempts: [{ taskId: "t1", n: 0, startedAt: "2026-10-07T10:00:00Z", latencyUs: 3000, neStatus: 201, outcome: "ok" }],
  audit: [{ tenant: "host-mno", id: "a1", at: "2026-10-07T10:00:00Z", actor: "api", actingForTenant: false, action: "order.created", objectType: "service_order", detail: { x: 1 } }],
};

export const nes: NE[] = [
  {
    tenant: "host-mno", id: "n1", code: "udr01", domain: "sba", nfType: "UDR", vendor: "V", shared: true, maxTps: 1500, maxConcurrency: 64, reservedPct: 20,
    endpoints: [{ adapterType: "sba", baseUri: "https://localhost:9102", weight: 100 }],
    access: { "mvno-alpha": { tenant: "mvno-alpha", quotaTps: 400, allowedOps: ["udr.*"] } },
    calls15m: 10, errors15m: 1, p50us: 3000, p99us: 9000, breaker: "closed",
  },
  {
    tenant: "mvno-alpha", id: "n2", code: "ocs-alpha", domain: "bss", nfType: "OCS", vendor: "V", shared: false, maxTps: 400, maxConcurrency: 32, reservedPct: 0,
    endpoints: [], access: {}, calls15m: 0, errors15m: 0, p50us: 0, p99us: 0, breaker: "",
  },
];

export const catalog: SpecSummary[] = [
  { tenant: "GLOBAL", spec: "AddVoNR", version: "1.1.0", status: "ACTIVE", txMode: "ATOMIC", priority: "p1", description: "Add IMS", tasks: 3, domains: ["ims"], file: "specs/mobile.yaml" },
  { tenant: "mvno-alpha", spec: "AddVoNR", version: "1.2.0-a", status: "ACTIVE", txMode: "ATOMIC", priority: "p1", description: "Alpha IMS", tasks: 2, domains: ["ims"], file: "specs/alpha.yaml" },
  { tenant: "GLOBAL", spec: "CreateL3VPN", version: "1.0.0", status: "ACTIVE", txMode: "BEST_EFFORT", priority: "p2", description: "VPN", tasks: 1, domains: ["netconf"], file: "specs/fixed.yaml" },
];

export const spec: SpecDetail = {
  spec: {
    id: "s1", tenant: "GLOBAL", spec: "AddVoNR", version: "1.1.0", status: "ACTIVE", txMode: "ATOMIC", priorityDefault: "P1", preempt: true, description: "Add IMS",
    params: { required: ["supi"], properties: { supi: { type: "string", pattern: "^imsi-[0-9]{15}$" }, cfu: { type: "string", maxLength: 15, enum: ["a"], minimum: 0, maximum: 9 } } },
    entityKeys: ["'supi:' + params.supi"],
    tasks: [
      { id: "impi", ne: { nfType: "IMS_PG" }, domain: "ims", op: "ims.impi.put", template: "ims/impi.json.tmpl", compensate: "ims.impi.delete" },
      { id: "impu", ne: { nfType: "IMS_PG" }, domain: "ims", op: "ims.impu.put", dependsOn: ["impi"], when: "has(params.cfu)", pivot: true, coalesceKey: "'k'", compensateTemplate: "ims/x.json.tmpl" },
    ],
    example: { supi: "imsi-416770000000001" },
  },
  file: "specs/mobile.yaml",
  source: "tenant: GLOBAL\nspec: AddVoNR\n",
  templates: { "ims/impi.json.tmpl": '{"impi": "{{.Params.supi}}"}' },
  versions: ["1.1.0"],
  others: ["mvno-alpha 1.2.0-a"],
};

export const lab: LabResult = {
  tenant: "mvno-alpha",
  specTenant: "mvno-alpha",
  spec: "AddVoNR",
  version: "1.2.0-a",
  txMode: "ATOMIC",
  priority: "p1",
  valid: true,
  problems: [],
  entityKeys: ["supi:imsi-1"],
  tasks: [
    { id: "impi", wave: 0, op: "ims.impi.put", domain: "ims", nfType: "IMS_PG", applies: true, dependsOn: [], ne: "imspg01", neTenant: "host-mno", shared: true, template: "ims/impi.json.tmpl", request: { impi: "x" }, compensate: "ims.impi.delete", compensateRequest: { del: 1 } },
    { id: "skip", wave: 1, op: "ims.x", domain: "ims", nfType: "IMS_PG", applies: false, when: "false", dependsOn: ["impi"], neError: "no NE", renderError: "bad template", compensateError: "bad comp", whenError: "rule" },
  ],
  order: { "@type": "ServiceOrder" },
};

export const dlq: DLQRecord[] = [
  { partition: 2, offset: 12, at: "2026-10-07T10:00:00Z", key: "host-mno|o1", sourceTopic: "", error: "", headers: { tenant: "host-mno", producer_app: "orchestrator" }, payload: { code: "DXPS-3404", operation: "udr.am.patch", order: "o1", message: "not found" } },
  { partition: 1, offset: 3, at: "2026-10-07T10:00:00Z", key: "k", sourceTopic: "dxps.bc.p1", error: "decode", headers: {}, text: "raw" },
];
