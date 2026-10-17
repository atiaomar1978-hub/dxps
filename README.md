# DxPS - Real-time Provisioning System

DxPS turns a business order such as *"create a 5G subscriber with VoNR"* into the exact sequence of network
commands needed to fulfil it. It runs those commands against the network elements (5G core, IMS, eSIM, charging,
number portability, fixed access, transport and network APIs) in real time. It tracks every step and rolls back
cleanly when something fails.

It is built for an operator that hosts **MVNOs and enterprise customers on the same platform**. Each tenant gets
isolated data, its own priorities and quotas, and its own variants of the provisioning recipes.

![Mosaic live dashboard](dxps/docs/mosaic.png)

---

## What it does

| Capability | Summary |
|---|---|
| **Northbound order API** | TMF641 Service Ordering (create / list / get / cancel) and TMF688 event-hub subscriptions over HTTPS, with OAuth2 tokens that carry the tenant. |
| **Order decomposition** | A YAML catalog describes each service as a DAG of network tasks with parameters, CEL conditions, dependencies and compensations. The planner resolves the tenant's own version first, then the global one. |
| **Real-time orchestration** | Tasks run in parallel where the DAG allows and in strict order where it doesn't. Work on the same subscriber or resource is serialised, so it never races. |
| **Transaction modes** | `ATOMIC` (all or nothing, with saga compensation in reverse order), `PARTIAL_ALLOWED` and `BEST_EFFORT`. |
| **Priority and fairness** | Four priority tiers. P0 is strict; P1-P3 are weighted 4:2:1. Tenants share each tier fairly (deficit round robin), so one noisy MVNO cannot starve the others. |
| **Resilient delivery** | Idempotent orders, exactly-once processing between Kafka and PostgreSQL (inbox, transactional outbox), a 5 s / 30 s / 5 min retry ladder, a dead-letter queue with re-drive, and per-NE circuit breakers and rate budgets. |
| **Southbound adapters** | Seven domain drivers: 5G SBA (UDM/UDR, PCF, SMF), IMS, NETCONF/RESTCONF, eSIM (GSMA ES2+), BSS/charging and NPDB, fixed access (TR-385 / TR-369 USP), and exposure (CAMARA / NEF). |
| **Event notifications** | Order state changes go to tenant webhooks as TMF688 events, signed with HMAC-SHA256, with bounded retries. |
| **Network simulators** | Realistic NE simulators over HTTP/2 with mutual TLS. They validate payloads, keep state per tenant, and support injected faults and latency, so the whole flow runs end to end without a real network. |
| **Mosaic dashboard** | A live operations view of tenants, priority lanes, Kafka topics and lag, orders, NE health and latency, payload exchanges, webhooks, security probes and test results. |
| **DxPS console** | A signed-in web console for operations and configuration: orders with their task graphs and payloads, dead letters, NE simulators and fault injection, the service catalog, a catalog studio for drafts, a Template Lab for dry runs, an order composer, tenant and NE inventory editing, reports, users and roles. See [DxPS console](#dxps-console). |

## How an order flows

```mermaid
flowchart LR
    C[BSS / CRM / MVNO portal] -->|TMF641 order<br/>OAuth2 + tenant| G[Gateway]
    G -->|BusinessCommand<br/>dxps.bc.P0..P3| O[Orchestrator]
    O -->|plan DAG from catalog| O
    O -->|NeTask<br/>dxps.task.domain.Pn| A[Adapters x7 domains]
    A -->|HTTP/2 mTLS| N[(Network elements<br/>or simulators)]
    A -->|TaskResult| O
    O -->|state + outbox| P[(PostgreSQL 18<br/>row-level security)]
    O -->|OrderEvent| E[Event hub]
    E -->|TMF688 webhook<br/>HMAC signed| C
    O -.->|metrics| M[Mosaic dashboard]
    U[Console UI] -->|BFF| K[Console API]
    K -->|read views, inventory edits| P
    K -->|orders and cancels| G
    K -.->|lag, DLQ, faults| A
```

1. The **gateway** authenticates the caller and gets the tenant from the token. It deduplicates on the client
   idempotency key, stores the order and publishes a `BusinessCommand` on the topic for its priority.
2. The **orchestrator** plans the order into a DAG of NE tasks using the tenant's catalog. It picks only network
   elements the tenant owns or is entitled to, then sends the ready tasks to the domain topics.
3. The **adapters** render the payload template and call the network element. They report back success, a
   transient error (retry), a rejection or downtime.
4. The orchestrator moves the DAG forward, retries through the retry ladder, or compensates already-completed steps
   in reverse order. It then sets the TMF641 order state (`acknowledged -> inProgress -> completed / failed / partial`).
5. The **event hub** sends each state change to the tenant's registered listeners.

## Services

| Service | Role | Default port |
|---|---|---|
| `gateway` | TMF641 / TMF688 northbound API (TLS 1.3, OAuth2 bearer tokens) | 8443 |
| `orchestrator` | Planning, DAG execution, retries, saga compensation, order state | 9201 (stats) |
| `adapter` | Southbound drivers for the 7 domains, NE budgets and circuit breakers | 9202 (stats) |
| `eventhub` | Signed TMF688 webhook delivery | 9203 (stats) |
| `netsim` | NE simulators: NRF, UDR, IMS, RESTCONF/USP, SM-DP+, OCS, NPDB, NEF/CAMARA, hub sink | 9101-9109, admin 9199 |
| `dashboard` | Mosaic live dashboard | 8088 |
| `console` | Console API: sessions and roles, cross-tenant views, inventory edits, catalog drafts, Template Lab, audit | 8446 (loopback, TLS) |
| console UI | Next.js web console (`dxps/console/`) in front of the console API | 8092 (TLS) |
| `dxpsctl` | Operations CLI: secrets, dev PKI, migrations, seeding, topics, tokens, DLQ re-drive, API calls, order trace, test report | - |

Infrastructure: **PostgreSQL 18** (TLS 1.3, SCRAM) and **Apache Kafka 4** (KRaft, no ZooKeeper).

## Provisioning catalog

| Service spec | Network tasks (simplified) |
|---|---|
| `CreateSubscriber5G` | UDM auth subscription -> AM data -> SMF selection -> PCF policy -> OCS account |
| `AddVoNR` | IMS private identity -> public identity -> MMTel service |
| `SuspendSubscriber` / `ChangePlan` / `DeleteSubscriber5G` | Barring, AM data plus bundle change, full ordered teardown |
| `ProvisionESIM` | SM-DP+ download order -> confirm order (cancel as compensation) |
| `PortInNumber` | NPDB port-in (delete as compensation) |
| `ActivateFTTH` | OLT/ONT YANG patch -> CPE via TR-369 USP |
| `CreateL3VPN` | PE router L3VPN via RESTCONF YANG patch |
| `QoDSession` | CAMARA Quality-on-Demand session via NEF |

Tenants can override any spec. For example, `mvno-alpha` runs its own OCS and adds a welcome bundle to
`CreateSubscriber5G`. No code changes are needed.

## DxPS console

The console is where people run and configure the platform. It is served at **https://localhost:8092**, and you
sign in with a console account. Each role includes the ones before it:

| Role | Can |
|---|---|
| `viewer` | See every page |
| `operator` | Also submit and cancel orders, inject NE faults, redrive dead letters, start load runs and probes |
| `configurator` | Also edit tenants, NE limits and entitlements, work on catalog drafts and use the Template Lab |
| `admin` | Also manage users |

![DxPS console dashboard](dxps/docs/console/dashboard.png)

| Area | Pages |
|---|---|
| **Operations** | **Dashboard** (service health, order and task volumes, state mix per tenant, NE latency, scheduler queues) · **Service orders** (filter and search; each order shows its task graph, the request and response for every NE task, attempts, the TMF641 request, audit, and Cancel) · **NE tasks** · **Dead letters** (inspect and redrive) · **Network & simulators** (NE health, simulator stats, fault injection, recent payload exchanges) · **Kafka** (priority lanes, consumer lag) |
| **Configuration** | **Service catalog** (each spec's task graph, parameters, templates and YAML) · **Catalog studio** (draft changes to specs and templates, validate, export) · **Template Lab** (dry run of one order item) · **Order composer** (real TMF641 orders through the gateway) · **Tenants** (SLA tier, weight, quota, status) · **NE inventory** (limits, entitlements on shared NEs) |
| **Insight** | **Reports** (orders per day, success rate, latency by spec and NE, top errors) · **Load & probes** · **Test results** · **Webhooks** · **Audit trail** (platform and console) |
| **Administration** | **Users** · **Settings** · **My account** |

**Template Lab.** It resolves the spec the way the orchestrator does: the tenant's override first, then GLOBAL. It
then validates the parameters, evaluates the `when` rules, entity keys and coalesce keys, and picks a network element
with the tenant's entitlements. Finally it renders every forward and compensation payload. Nothing is published and
no NE is called. A valid result can be sent on to the Order composer.

![Template Lab](dxps/docs/console/template-lab.png)

**Catalog changes.** The services run with the catalog compiled in, so catalog changes go through a draft:

1. Create a draft from catalog files, or from a spec with **Create override draft** for a tenant.
2. Edit it in the **Catalog studio**.
3. **Validate** it. The whole catalog is loaded with your changes and each spec's example is run through the Lab.
4. Try it in the **Template Lab**.
5. **Export** the changed files into `dxps/internal/catalog/data`, then rebuild and restart the services.

Tenant, NE limit and entitlement edits apply live within about 10 seconds. `run-all` re-seeds the demo tenants and
inventory when it starts.

| Order detail | Catalog spec |
|---|---|
| ![Order detail](dxps/docs/console/order-detail.png) | ![Catalog spec](dxps/docs/console/catalog-spec.png) |
| **Catalog studio** | **NE inventory** |
| ![Catalog studio](dxps/docs/console/catalog-studio.png) | ![NE inventory](dxps/docs/console/ne-inventory.png) |
| **Reports** | **Dead letters** |
| ![Reports](dxps/docs/console/reports.png) | ![Dead letters](dxps/docs/console/dead-letters.png) |

How it is built:

- **Console API (`dxps/cmd/console`, `dxps/internal/console`).**
  - It listens on loopback only, with a certificate issued from the DxPS development CA.
  - It reads the database with the operations role. Inventory edits run in a transaction and write a DxPS
    audit event with actor `console:<user>`.
  - Orders are submitted through the real gateway with a 2-minute token for the tenant, limited to order read and
    write.
  - Users are stored with PBKDF2-SHA256. Five failed sign-ins lock the account for a while, and sessions last 8 hours.
- **Console UI (`dxps/console`).**
  - Next.js with a backend-for-frontend: the browser only talks to the UI origin.
  - The session token is kept in an HttpOnly, SameSite=Strict cookie, and every change needs a CSRF header.

## Multi-tenancy

- Every table carries `tenant varchar(40)` with a composite primary key `(tenant, id uuid)`.
- PostgreSQL **row-level security** isolates tenants. The application role cannot bypass it; only a separate
  operations role can.
- Tenants can be a full MVNO, a light MVNO or an enterprise, each with its own quotas, priorities, NE
  entitlements and catalog variants. Suspended tenants are rejected at the gateway.
- Seed tenants: `host-mno`, `mvno-alpha`, `mvno-beta`, `ent-acme`, `mvno-gamma` (suspended).

## Security

- TLS 1.3 everywhere, and **mutual TLS** to every network element (ECDSA P-256 development CA).
- Strict JWT verification: HS256 only, no `alg=none`, and `iss`, `aud`, `exp`, `nbf` and tenant are required.
  Scopes are enforced per endpoint.
- Webhook secrets are encrypted at rest with AES-256-GCM, bound to the tenant. Callbacks must use https, be
  allow-listed and cannot follow redirects.
- Sensitive values are redacted in logs and payload traces. Secrets are generated locally and never committed.
- Scanned with `govulncheck` (no vulnerabilities) and `gosec` (no high or medium findings).

## Quality and results

| Metric | Result |
|---|---|
| Unit, integration and E2E tests | 177 / 177 pass (Windows and Ubuntu 24.04) |
| Statement coverage | 91.5 % of 4,196 statements |
| Live security and behaviour probes | 24 / 24 pass |
| Load run | 2,000 orders at 100 orders/s with 10 % injected NE faults: all accepted, outbox drained to 0 |

Full details, payloads and evidence: `DxPS_Test_Cases_and_Results.pdf`.

## Repository layout

| Path | Contents |
|---|---|
| `dxps/cmd/` | Service binaries and the `dxpsctl` CLI |
| `dxps/internal/` | Packages (gateway, orchestrator, planner, scheduler, keylane, saga, bus, store, adapter, netsim, dashboard, auth, pki, ...) with their tests |
| `dxps/internal/console/` | Console API: data views, live service clients, users, drafts, Template Lab |
| `dxps/console/` | Console web UI (Next.js) and its tests |
| `dxps/internal/catalog/data/` | Service specs (YAML) and NE payload templates |
| `dxps/internal/store/migrations/` | PostgreSQL schema (tenant composite keys, RLS, partitions) |
| `dxps/scripts/` | Install / setup / run / test / security-scan scripts: `.ps1` for Windows, `.sh` for Ubuntu and macOS |
| `dxps/INSTALL.md` | Step-by-step installation and running guide |
| `dxps/docs/COMPONENTS.md` | How PostgreSQL, Kafka and each component are used, with real rows and messages |
| `DxPS_Components_and_Data_Guide.*` | Components and data guide (Word / PDF) |
| `DxPS_TO-BE_HLD_LLD.*` | TO-BE design, high- and low-level (Word / PDF) |
| `DxPS_Test_Cases_and_Results.*` | Test cases and results (Word / PDF) |
| `DxPS_Installation_and_Running_Guide.*` | Installation guide (Word / PDF) |
| `design/` | Generators for the documents and diagrams, and the delivery packager |

## Quick start

```bash
# Ubuntu 22.04/24.04 or macOS 13+ (normal user with sudo)
cd dxps
bash scripts/install-deps-ubuntu.sh   # or: bash scripts/install-deps-macos.sh
bash scripts/infra-setup.sh
bash scripts/run-all.sh
```

```powershell
# Windows 10/11
cd dxps
powershell -ExecutionPolicy Bypass -File scripts\install-deps-windows.ps1
powershell -ExecutionPolicy Bypass -File scripts\infra-setup.ps1
powershell -ExecutionPolicy Bypass -File scripts\run-all.ps1
```

Then open the Mosaic dashboard at http://127.0.0.1:8088. Run the tests with `scripts/test-all` and the security
scans with `scripts/sec-scan`, and stop everything with `scripts/infra-stop`. See `dxps/INSTALL.md` for the full
guide, including how to submit a sample order.

To start the console (Ubuntu or macOS, Node.js 22.12+), run this once the platform is up:

```bash
cd dxps
bash scripts/console-run.sh                              # builds and starts the console API (8446) and UI (8092)
~/dxps-runtime/bin/console useradd -name admin -roles admin   # first user; the password is read from stdin
```

Then open https://localhost:8092 and sign in. Stop the console with `bash scripts/console-stop.sh`. There is no
Windows script for the console yet.
