// JSON shapes of the console API (dxps/internal/console).

export interface Count {
  tenant: string;
  key: string;
  sub?: string;
  n: number;
}

export interface NELatency {
  ne: string;
  calls: number;
  p50us: number;
  p99us: number;
  errors: number;
}

export interface RecentOrder {
  tenant: string;
  id: string;
  externalId: string;
  state: string;
  priority: number;
  spec: string;
  at: string;
  durationMs: number;
}

export interface Stats {
  ordersByState: Count[];
  tasksByDomain: Count[];
  neLatency: NELatency[];
  recent: RecentOrder[];
  outboxBacklog: number;
  ordersLastMin: number;
  tasksLastMin: number;
  p99OrderMs: number;
}

export interface Tenant {
  tenant: string;
  name: string;
  tenantType: string;
  hostTenant?: string;
  slaTier: string;
  slaWeight: number;
  quotaTps: number;
  status: string;
  orders: number;
  failed: number;
  active: number;
  lastOrder?: string;
  ownNes: number;
  entitledNes: number;
}

export interface TopicStat {
  topic: string;
  end: number;
}

export interface GroupLag {
  group: string;
  lag: number;
  state: string;
}

export interface Kafka {
  topics: TopicStat[];
  lags: GroupLag[];
}

export interface Scheduler {
  queued: number[];
  served: number[];
  byTenant: Record<string, unknown>;
}

export interface OrchestratorStats {
  bcLanes: { Lanes: number; InFlight: number; MaxLaneDepth: number };
  bcScheduler: Scheduler;
  resultLanes: { Lanes: number; InFlight: number; MaxLaneDepth: number };
  resultScheduler: Scheduler;
}

export interface AdapterNE {
  ne: string;
  calls: number;
  errors: number;
  lastUs: number;
  totalUs: number;
  breaker: string;
}

export interface Overview {
  at: string;
  up?: Record<string, boolean>;
  db?: Stats;
  tenants?: Tenant[];
  services?: { orchestrator?: OrchestratorStats; adapter?: AdapterNE[]; eventhub?: { delivered: number; failed: number } };
  kafka?: Kafka;
  errors: Record<string, string>;
}

export interface OrderRow {
  tenant: string;
  id: string;
  externalId: string;
  state: string;
  priority: number;
  txMode: string;
  channel: string;
  category: string;
  specs: string;
  tasks: number;
  requestedAt: string;
  completedAt?: string;
  durationMs: number;
  errorCode?: string;
  error?: string;
}

export interface OrderItem {
  id: string;
  itemId: string;
  spec: string;
  specVersion: string;
  specTenant: string;
  action: string;
  state: string;
  priority: number;
  txMode: string;
  entityKey: string;
  params: Record<string, unknown>;
  dependsOn: string[];
  errorCode?: string;
  error?: string;
  createdAt: string;
  updatedAt: string;
}

export interface OrderTask {
  id: string;
  itemIds: string[];
  specTask: string;
  ne: string;
  neTenant: string;
  domain: string;
  operation: string;
  state: string;
  attempt: number;
  seq: number;
  compensation: boolean;
  compensatesId?: string;
  afterPivot: boolean;
  coalesceKey?: string;
  request?: unknown;
  response?: unknown;
  neStatus?: number;
  neCode?: string;
  lastError?: string;
  createdAt: string;
  dispatchedAt?: string;
  completedAt?: string;
  dependsOn: string[];
  idempotencyKey: string;
}

export interface Attempt {
  taskId: string;
  n: number;
  startedAt: string;
  latencyUs: number;
  neStatus?: number;
  neCode?: string;
  outcome: string;
  error?: string;
}

export interface AuditRow {
  tenant: string;
  id: string;
  at: string;
  actor: string;
  actingForTenant: boolean;
  action: string;
  objectType: string;
  objectId?: string;
  detail?: unknown;
}

export interface ConsoleAudit {
  at: string;
  actor: string;
  action: string;
  object?: string;
  detail?: unknown;
  source: string;
}

export interface OrderDetail extends Omit<OrderRow, "tasks"> {
  idempotencyKey: string;
  items: unknown;
  businessCommands: OrderItem[];
  tasks: OrderTask[];
  attempts: Attempt[];
  audit: AuditRow[];
}

export interface TaskRow {
  tenant: string;
  id: string;
  orderId: string;
  externalId: string;
  ne: string;
  domain: string;
  operation: string;
  state: string;
  attempt: number;
  priority: number;
  compensation: boolean;
  neStatus?: number;
  neCode?: string;
  lastError?: string;
  updatedAt: string;
  completedAt?: string;
}

export interface Access {
  tenant: string;
  quotaTps: number;
  allowedOps: string[];
  subscriberGroup?: string;
}

export interface NE {
  tenant: string;
  id: string;
  code: string;
  domain: string;
  nfType: string;
  vendor: string;
  shared: boolean;
  maxTps: number;
  maxConcurrency: number;
  reservedPct: number;
  endpoints: { adapterType: string; baseUri: string; weight: number }[];
  access: Record<string, Access>;
  calls15m: number;
  errors15m: number;
  p50us: number;
  p99us: number;
  breaker: string;
}

export interface SpecSummary {
  tenant: string;
  spec: string;
  version: string;
  status: string;
  txMode: string;
  priority: string;
  description: string;
  tasks: number;
  domains: string[];
  file: string;
}

export interface ParamSchema {
  type: string;
  pattern?: string;
  enum?: string[];
  maxLength?: number;
  minimum?: number;
  maximum?: number;
}

export interface TaskSpec {
  id: string;
  ne: { nfType: string };
  domain: string;
  op: string;
  template?: string;
  compensate?: string;
  compensateTemplate?: string;
  dependsOn?: string[];
  when?: string;
  coalesceKey?: string;
  pivot?: boolean;
}

export interface Spec {
  id: string;
  tenant: string;
  spec: string;
  version: string;
  status: string;
  txMode: string;
  priorityDefault: string;
  preempt: boolean;
  description: string;
  params: { required: string[]; properties: Record<string, ParamSchema> };
  entityKeys: string[];
  tasks: TaskSpec[];
  example?: Record<string, unknown>;
}

export interface SpecDetail {
  spec: Spec;
  file: string;
  source: string;
  templates: Record<string, string>;
  versions: string[];
  others: string[] | null;
}

export interface DraftSummary {
  id: string;
  name: string;
  author: string;
  created: string;
  updated: string;
  files: string[];
  deleted?: string[];
}

export interface Draft {
  id: string;
  name: string;
  author: string;
  created: string;
  updated: string;
  files: Record<string, string>;
  deleted?: string[];
}

export interface SmokeResult {
  tenant: string;
  spec: string;
  version: string;
  ok: boolean;
  problems?: string[];
}

export interface Validation {
  ok: boolean;
  error?: string;
  specs: SpecSummary[];
  smoke: SmokeResult[];
  changed: string[];
}

export interface LabTask {
  id: string;
  wave: number;
  op: string;
  domain: string;
  nfType: string;
  when?: string;
  applies: boolean;
  whenError?: string;
  dependsOn: string[];
  ne?: string;
  neTenant?: string;
  shared?: boolean;
  subscriberGroup?: string;
  neError?: string;
  coalesceKey?: string;
  pivot?: boolean;
  template?: string;
  request?: unknown;
  renderError?: string;
  compensate?: string;
  compensateTemplate?: string;
  compensateRequest?: unknown;
  compensateError?: string;
}

export interface LabResult {
  tenant: string;
  specTenant?: string;
  spec: string;
  version?: string;
  txMode?: string;
  priority?: string;
  valid: boolean;
  problems: string[];
  entityKeys: string[];
  tasks: LabTask[];
  order?: unknown;
}

export interface DLQRecord {
  partition: number;
  offset: number;
  at: string;
  key: string;
  sourceTopic: string;
  error: string;
  headers: Record<string, string>;
  payload?: Record<string, unknown>;
  text?: string;
}

export interface SimStat {
  name: string;
  port: number;
  calls: number;
  byStatus: Record<string, number>;
  p50us: number;
  p99us: number;
  faults: number;
  latencyMs: number;
  errorRate: number;
  objects: number;
}

export interface Exchange {
  sim: string;
  at: string;
  tenant: string;
  method: string;
  path: string;
  proto: string;
  status: number;
  request?: unknown;
  response?: unknown;
  latUs: number;
  fault?: string;
}

export interface DemoStatus {
  running: boolean;
  started: string;
  target: number;
  sent: number;
  byStatus: Record<string, number>;
  byCode: Record<string, number>;
  bySpec: Record<string, number>;
  p50ms: number;
  p99ms: number;
  lastError?: string;
  probing: boolean;
}

export interface Probe {
  id: string;
  category: string;
  name: string;
  expect: string;
  got: string;
  pass: boolean;
}

export interface Point {
  t: number;
  orders: number;
  tasks: number;
  p99: number;
  lag: number;
}

export interface LoadGen {
  demo: DemoStatus;
  probes: Probe[] | null;
  history: Point[] | null;
}

export interface TestReport {
  available?: boolean;
  generatedAt?: string;
  elapsedSec?: number;
  totals?: { tests: number; passed: number; failed: number; skipped: number };
  coverage?: number;
  statements?: number;
  packages?: { name: string; tests: number; passed: number; failed: number; skipped: number; coverage: number }[];
  results?: { package: string; test: string; action: string; elapsed: number }[];
}

export interface Hub {
  tenant: string;
  id: string;
  callback: string;
  query?: string;
  secretRef: string;
  status: string;
  createdAt: string;
}

export interface Report {
  days: number;
  byDay: { day: string; tenant: string; state: string; n: number }[];
  bySpec: { spec: string; items: number; completed: number; failed: number; p50ms: number; p95ms: number }[];
  byNe: NELatency[];
  errors: { code: string; tenant: string; n: number; sample: string }[];
  tasksByDomain: Count[];
}

export interface User {
  name: string;
  roles: string[];
  disabled: boolean;
  created: string;
  lastLogin?: string;
}
