"""Box/arrow, sequence and state diagrams for the DxPS TO-BE design document."""
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
matplotlib.rcParams["text.parse_math"] = False
import matplotlib.pyplot as plt
from matplotlib.patches import FancyArrowPatch, FancyBboxPatch

OUT = Path(__file__).with_name("diagrams")
OUT.mkdir(exist_ok=True)

C = {
    "client": "#DBEAFE",
    "edge": "#E0E7FF",
    "kafka": "#FDE68A",
    "svc": "#D1FAE5",
    "db": "#FCE7F3",
    "ne": "#E5E7EB",
    "legacy": "#FECACA",
    "obs": "#EDE9FE",
}


def _edge_point(box, toward):
    x, y, w, h = box[:4]
    cx, cy = x + w / 2, y + h / 2
    dx, dy = toward[0] - cx, toward[1] - cy
    if dx == 0 and dy == 0:
        return cx, cy
    tx = (w / 2) / abs(dx) if dx else float("inf")
    ty = (h / 2) / abs(dy) if dy else float("inf")
    t = min(tx, ty)
    return cx + dx * t, cy + dy * t


def box_diagram(name, boxes, arrows, size=(13, 7), xlim=(0, 100), ylim=(0, 60), title=None, groups=()):
    fig, ax = plt.subplots(figsize=size, dpi=160)
    ax.set_xlim(*xlim)
    ax.set_ylim(*ylim)
    ax.axis("off")
    for gx, gy, gw, gh, glabel in groups:
        ax.add_patch(FancyBboxPatch((gx, gy), gw, gh, boxstyle="round,pad=0.3,rounding_size=1.2",
                                    fc="none", ec="#6B7280", lw=1.0, ls="--"))
        ax.text(gx + 0.6, gy + gh - 0.4, glabel, fontsize=8, va="top", ha="left", color="#374151", style="italic")
    for key, (x, y, w, h, label, color) in boxes.items():
        ax.add_patch(FancyBboxPatch((x, y), w, h, boxstyle="round,pad=0.2,rounding_size=0.8",
                                    fc=C.get(color, color), ec="#1F2937", lw=1.0))
        ax.text(x + w / 2, y + h / 2, label, ha="center", va="center", fontsize=7.5)
    for a in arrows:
        src, dst = a[0], a[1]
        label = a[2] if len(a) > 2 else ""
        style = a[3] if len(a) > 3 else "-"
        b1, b2 = boxes[src], boxes[dst]
        c1 = (b1[0] + b1[2] / 2, b1[1] + b1[3] / 2)
        c2 = (b2[0] + b2[2] / 2, b2[1] + b2[3] / 2)
        p1 = _edge_point(b1, c2)
        p2 = _edge_point(b2, c1)
        ax.add_patch(FancyArrowPatch(p1, p2, arrowstyle="-|>", mutation_scale=11, lw=1.0,
                                     color="#111827", ls=style, shrinkA=2, shrinkB=2))
        if label:
            mx, my = (p1[0] + p2[0]) / 2, (p1[1] + p2[1]) / 2
            ax.text(mx, my, label, fontsize=6.5, ha="center", va="center",
                    bbox=dict(fc="white", ec="none", alpha=0.85, pad=0.6))
    if title:
        ax.set_title(title, fontsize=11, weight="bold")
    path = OUT / f"{name}.png"
    fig.savefig(path, bbox_inches="tight")
    plt.close(fig)
    return path


def sequence_diagram(name, actors, messages, size=(13, 8), title=None):
    fig, ax = plt.subplots(figsize=size, dpi=160)
    n = len(actors)
    total = len(messages) + 2
    ax.set_xlim(0, n)
    ax.set_ylim(0, total)
    ax.axis("off")
    xs = {a: i + 0.5 for i, a in enumerate(actors)}
    for a, x in xs.items():
        ax.add_patch(FancyBboxPatch((x - 0.42, total - 0.9), 0.84, 0.7, boxstyle="round,pad=0.02",
                                    fc=C["svc"], ec="#1F2937"))
        ax.text(x, total - 0.55, a, ha="center", va="center", fontsize=7, weight="bold")
        ax.plot([x, x], [0.2, total - 0.9], color="#9CA3AF", lw=0.8, ls="--")
    for i, m in enumerate(messages):
        src, dst, label = m[0], m[1], m[2]
        dashed = len(m) > 3 and m[3]
        y = total - 1.5 - i
        if src == dst:
            x = xs[src]
            ax.add_patch(FancyArrowPatch((x, y + 0.15), (x, y - 0.25), connectionstyle="arc3,rad=-1.6",
                                         arrowstyle="-|>", mutation_scale=9, lw=0.9, color="#111827"))
            ax.text(x + 0.12, y, label, fontsize=6.3, va="center", ha="left")
            continue
        ax.add_patch(FancyArrowPatch((xs[src], y), (xs[dst], y), arrowstyle="-|>", mutation_scale=9,
                                     lw=0.9, color="#1D4ED8" if dashed else "#111827",
                                     ls="--" if dashed else "-"))
        ax.text((xs[src] + xs[dst]) / 2, y + 0.18, label, fontsize=6.3, ha="center", va="bottom")
    if title:
        ax.set_title(title, fontsize=11, weight="bold")
    path = OUT / f"{name}.png"
    fig.savefig(path, bbox_inches="tight")
    plt.close(fig)
    return path


def build_all():
    paths = {}

    # ---------------- AS-IS ----------------
    paths["asis"] = box_diagram(
        "01_asis_flow",
        {
            "crm": (1, 44, 12, 8, "CRM / Billing DB\n(business txns)", "client"),
            "pil": (18, 44, 14, 8, "PIL dispatcher\n(Pro*C, poll loop)\nLOAD / UPDATE", "legacy"),
            "if": (37, 44, 16, 8, "Oracle\nPRV_INTERFACE\n(status 10/15/20/30/40)", "db"),
            "mml": (58, 44, 15, 8, "MMLGEN daemon\nPRV_V_MML_SETUP\n$$PARAM$$ templates", "legacy"),
            "q": (78, 44, 20, 8, "Oracle\nPRV_SIM_MML_TRANSACTIONS\n(status 9/10/20/30/40)", "db"),
            "sim": (66, 26, 18, 9, "SIM / HLRCOMM instances\n(process per NE/queue)\nCTP-TCP | MTP-X.25 |\nATP-async | PTP-FIFO | X.29", "legacy"),
            "bb": (1, 26, 14, 8, "BBBroker\nTCP XML -> gSOAP\nsubmitSync", "legacy"),
            "npm": (17, 26, 15, 8, "NPM broker + listener\ngSOAP doProvisioning /\nsetProvisioningStatus", "legacy"),
            "smsc": (34, 26, 13, 8, "SMSC (Java)\nOpenSMPP bind/submit\nOracle poll", "legacy"),
            "corba": (49, 26, 15, 8, "CORBA NE adaptor\nOrbix IIOP\nInvokeCommand(xml)", "legacy"),
            "ne1": (66, 8, 18, 9, "HLR / AuC / IN / VMS\nAXE, Huawei IN,\nAlcatel HLR (MML)", "ne"),
            "ne2": (1, 8, 14, 8, "BlackBerry BES\n(Axis SOAP)", "ne"),
            "ne3": (17, 8, 15, 8, "Nokia Profile\nManager (SOAP)", "ne"),
            "ne4": (34, 8, 13, 8, "SMSC", "ne"),
            "ne5": (49, 8, 15, 8, "CORBA NEs /\nInventory / VoMS", "ne"),
        },
        [
            ("crm", "pil", "SELECT (poll)"),
            ("pil", "if", "INSERT"),
            ("if", "mml", "FOR UPDATE\nmin(priority)"),
            ("mml", "q", "INSERT 1:N"),
            ("q", "sim", "poll per\nNE/queue"),
            ("sim", "ne1", "MML"),
            ("bb", "ne2"), ("npm", "ne3"), ("smsc", "ne4"), ("corba", "ne5"),
        ],
        title="AS-IS: DB-as-queue provisioning (CRM -> PIL -> MMLGEN -> SIM -> NE)",
    )

    # ---------------- TO-BE context / logical ----------------
    paths["tobe_logical"] = box_diagram(
        "02_tobe_logical",
        {
            "crm": (1, 50, 14, 7, "Host MNO CRM / COM\n(TMF622 -> TMF641)", "client"),
            "ext": (1, 40, 14, 7, "MVNO BSS tenants\nPartners (CAMARA)", "client"),
            "gw": (19, 44, 15, 10, "DxPS API Gateway (Go)\nTMF641/652/702 REST\ngRPC, OAuth2/mTLS\ntenant from token\n-> Kafka producer", "edge"),
            "kbc": (38, 44, 17, 10, "Kafka\ndxps.bc.p0 .. p3\n(priority tiers, key =\ntenant|entity_key)", "kafka"),
            "orc": (59, 44, 19, 10, "DxPS Orchestrator (Go)\ntenant-aware sequencer\ndecomposition, DAG, saga\n(many-to-many)", "svc"),
            "pg": (82, 44, 17, 10, "PostgreSQL (RLS)\nPK (tenant, id)\norders, plans, tasks,\ncatalog, outbox, inbox", "db"),
            "cat": (82, 31, 17, 8, "Catalog Service\nTMF633/634\ndecomposition rules", "svc"),
            "inv": (82, 20, 17, 8, "Inventory Service\nTMF638/639", "svc"),
            "ktask": (59, 29, 19, 9, "Kafka\ndxps.task.<domain>.p0..p3\nkey = tenant|ne_id|entity_key", "kafka"),
            "kres": (38, 29, 17, 9, "Kafka\ndxps.task.result\ndxps.retry.* / dxps.dlq", "kafka"),
            "evt": (19, 29, 15, 9, "Event Hub (Go)\nTMF688 notifications\nwebhooks / Kafka", "edge"),
            "a5": (21, 10, 14, 9, "BSS adapters\nOCS/CHF REST\nTMF654/666, NPDB", "svc"),
            "a1": (37, 10, 14, 9, "5G Core adapter\nHTTP/2 SBA\nUDM/UDR/PCF/\nNSSF/NEF/SMSF", "svc"),
            "a2": (53, 10, 14, 9, "IMS / VoNR\nadapter\nHSS-PG, TAS,\nENUM REST", "svc"),
            "a3": (69, 10, 14, 9, "Network config\nNETCONF/RESTCONF\ngNMI, O-RAN O1,\n28.532 MnS", "svc"),
            "a4": (85, 10, 14, 9, "Access & device\neSIM SGP.22/.32\nBBF TR-369 USP\nTR-385 OLT", "svc"),
            "net": (21, 0, 78, 6, "Telco networks: 5G SA / 5G-Advanced core, 4G EPC (NSA), IMS, transport/IP, RAN, FTTH, eSIM, OSS", "ne"),
            "obs": (1, 18, 16, 8, "Observability\nOpenTelemetry, Prometheus\nTempo/Jaeger, Loki", "obs"),
        },
        [
            ("crm", "gw"),
            ("ext", "gw"),
            ("gw", "kbc", "idempotent\nacks=all"),
            ("kbc", "orc", "consume\nread_committed"),
            ("orc", "pg", "ACID tx\n+ outbox"),
            ("orc", "cat"),
            ("orc", "ktask", "ready tasks"),
            ("ktask", "a1"), ("ktask", "a2"), ("ktask", "a3"), ("ktask", "a4"), ("ktask", "a5"),
            ("a1", "kres", "", "--"),
            ("kres", "orc", "results"),
            ("orc", "inv", "state sync"),
            ("orc", "evt", "state change"),
            ("evt", "crm", "TMF688 events"),
            ("a1", "net"), ("a2", "net"), ("a3", "net"), ("a4", "net"), ("a5", "net"),
        ],
        size=(14, 8.2),
        title="DxPS logical architecture (multi-tenant MVNO, Kafka-first, Go + PostgreSQL)",
    )

    # ---------------- Kafka topology ----------------
    paths["kafka"] = box_diagram(
        "03_kafka_topology",
        {
            "p0": (2, 46, 18, 8, "dxps.bc.p0 (critical)\n24 partitions\nbarring, fraud, LI\nkey = tenant|entity_key", "kafka"),
            "p1": (2, 36, 18, 8, "dxps.bc.p1 (interactive)\n48 partitions\nshop / app activation", "kafka"),
            "p2": (2, 26, 18, 8, "dxps.bc.p2 (standard)\n96 partitions\nCRM orders", "kafka"),
            "p3": (2, 16, 18, 8, "dxps.bc.p3 (bulk)\n48 partitions\nmigration, campaigns", "kafka"),
            "sched": (26, 26, 20, 18, "DxPS Orchestrator\n4 consumer groups\n(one per tier)\n\nWeighted-fair scheduler\ntiers 8 : 4 : 2 : 1 + aging\nper-tenant DRR fairness\npause/resume backpressure", "svc"),
            "pg": (26, 4, 20, 12, "PostgreSQL\ntenant entity sequencer\nplan / task / outbox\n(one ACID tx per BC)", "db"),
            "tasks": (52, 30, 21, 26, "Domain task topics\n(each x 4 priority tiers)\n\ndxps.task.sba.p{0..3}\ndxps.task.ims.p{0..3}\ndxps.task.netconf.p{0..3}\ndxps.task.esim.p{0..3}\ndxps.task.bss.p{0..3}\ndxps.task.access.p{0..3}\n\nkey = tenant|ne_id|entity_key", "kafka"),
            "res": (52, 4, 21, 12, "dxps.task.result\n96 partitions\nkey = tenant|order_id", "kafka"),
            "retry": (78, 46, 20, 12, "dxps.retry.5s\ndxps.retry.30s\ndxps.retry.5m\ndxps.dlq (manual)", "kafka"),
            "ad": (78, 28, 20, 12, "Adapter worker pools\n(key-lane parallelism,\nper-NE + per-tenant\ntoken buckets)", "svc"),
            "state": (78, 4, 20, 14, "dxps.state.order (compacted)\ndxps.state.ne-health\n(compacted)\ndxps.event.tmf688", "kafka"),
        },
        [
            ("p0", "sched"), ("p1", "sched"), ("p2", "sched"), ("p3", "sched"),
            ("sched", "pg", "tx"),
            ("pg", "tasks", "outbox\nfast-publish"),
            ("tasks", "ad", "consume"),
            ("ad", "retry", "retryable", "--"),
            ("ad", "res", "result"),
            ("res", "sched", "next DAG nodes"),
            ("sched", "state", "", "--"),
        ],
        size=(14, 7.5),
        title="Kafka topology: priority tiers, domain task topics, retry ladder, compacted state",
    )

    # ---------------- Saga sequence ----------------
    paths["seq"] = sequence_diagram(
        "04_sequence_activation",
        ["MVNO BSS", "DxPS API GW", "Kafka bc.p1", "Orchestrator", "PostgreSQL", "Kafka task", "SBA adapter", "UDM/UDR", "Kafka result", "Event Hub"],
        [
            ("MVNO BSS", "DxPS API GW", "POST /serviceOrder (TMF641, token tenant=mvno-a, Idempotency-Key)"),
            ("DxPS API GW", "Kafka bc.p1", "produce BusinessCommand key=tenant|entity_key (acks=all, idempotent)"),
            ("DxPS API GW", "MVNO BSS", "201 ServiceOrder state=acknowledged", True),
            ("Kafka bc.p1", "Orchestrator", "poll (read_committed)"),
            ("Orchestrator", "PostgreSQL", "BEGIN; SET LOCAL app.tenant; inbox dedupe; entity_seq check; plan DAG; tasks + outbox; COMMIT"),
            ("Orchestrator", "Kafka task", "fast-publish READY tasks (key=tenant|ne_id|entity_key)"),
            ("Orchestrator", "PostgreSQL", "mark outbox published; commit consumer offset"),
            ("Kafka task", "SBA adapter", "poll -> key-lane worker"),
            ("SBA adapter", "UDM/UDR", "PUT /nudr-dr/v2/subscription-data/{ueId}/... (HTTP/2, OAuth2)"),
            ("UDM/UDR", "SBA adapter", "201/204", True),
            ("SBA adapter", "Kafka result", "TaskResult SUCCEEDED (transactional: result + offset)"),
            ("Kafka result", "Orchestrator", "consume result"),
            ("Orchestrator", "PostgreSQL", "task SUCCEEDED; unlock dependants; outbox next tasks"),
            ("Orchestrator", "Orchestrator", "repeat until DAG terminal (or compensate)"),
            ("Orchestrator", "Event Hub", "ServiceOrderStateChangeEvent completed"),
            ("Event Hub", "MVNO BSS", "TMF688 notification to the tenant's own hub listener", True),
        ],
        size=(15, 8.5),
        title="DxPS sequence: MVNO business command -> Kafka -> orchestrated saga -> 5G core -> notification",
    )

    # ---------------- Many-to-many DAG ----------------
    paths["dag"] = box_diagram(
        "05_dag_m2n",
        {
            "bc1": (1, 44, 18, 7, "BC-1 CreateSubscriber\n(SUPI, MSISDN)", "client"),
            "bc2": (1, 33, 18, 7, "BC-2 AddService VoNR", "client"),
            "bc3": (1, 22, 18, 7, "BC-3 SetDataPlan 5G-100GB\n(policy + slice)", "client"),
            "bc4": (1, 11, 18, 7, "BC-4 Download eSIM profile", "client"),
            "t1": (30, 48, 22, 6, "T1 UDR auth subscription\n(Nudr provisioned data)", "svc"),
            "t2": (30, 39, 22, 6, "T2 UDR AM + SMF-sel + SMS data\n(coalesced BC-1 + BC-2 + BC-3)", "svc"),
            "t3": (30, 30, 22, 6, "T3 IMS HSS-PG IMPI/IMPU\n+ TAS MMTel profile", "svc"),
            "t4": (30, 21, 22, 6, "T4 PCF policy data (UDR)\n+ NSSAI slice entitlement", "svc"),
            "t5": (30, 12, 22, 6, "T5 OCS/CHF account + bundle\n(TMF654 / vendor REST)", "svc"),
            "t6": (30, 3, 22, 6, "T6 SM-DP+ ES2+ DownloadOrder\n+ ConfirmOrder", "svc"),
            "ne1": (64, 42, 16, 9, "UDM / UDR\n(5G SA)", "ne"),
            "ne2": (64, 29, 16, 8, "IMS core\n(HSS, TAS)", "ne"),
            "ne3": (64, 17, 16, 8, "PCF / OCS / CHF", "ne"),
            "ne4": (64, 4, 16, 8, "SM-DP+", "ne"),
            "note": (83, 14, 16, 34, "Many-to-many:\n\nBC -> N tasks\n(BC-3 -> T2, T4, T5)\n\nTask <- N BCs\n(T2 serves BC-1,2,3)\n\nTask DAG edges:\nT1 -> T2 -> T3\nT1 -> T4\nT1 -> T6\nT2 -> T5", "obs"),
        },
        [
            ("bc1", "t1"), ("bc1", "t2"), ("bc2", "t2"), ("bc2", "t3"),
            ("bc3", "t2"), ("bc3", "t4"), ("bc3", "t5"), ("bc4", "t6"),
            ("t1", "ne1"), ("t2", "ne1"), ("t3", "ne2"), ("t4", "ne3"), ("t5", "ne3"), ("t6", "ne4"),
        ],
        size=(14, 7.2),
        title="Decomposition: many-to-many business commands <-> network tasks (DAG)",
    )

    # ---------------- Task state machine ----------------
    paths["fsm"] = box_diagram(
        "06_task_fsm",
        {
            "planned": (2, 40, 13, 7, "PLANNED\n(AS-IS 9)", "svc"),
            "ready": (20, 40, 13, 7, "READY\n(AS-IS 10)", "svc"),
            "disp": (38, 40, 14, 7, "DISPATCHED\n(outbox published)", "svc"),
            "inflight": (57, 40, 14, 7, "IN_FLIGHT\n(AS-IS 20)", "svc"),
            "ok": (78, 48, 16, 7, "SUCCEEDED\n(AS-IS 30)", "client"),
            "retry": (57, 24, 14, 7, "RETRY_WAIT\n(backoff + jitter)", "kafka"),
            "fail": (78, 32, 16, 7, "FAILED\n(AS-IS 40)", "legacy"),
            "comp": (57, 8, 14, 7, "COMPENSATING\n(AS-IS 49/50/95/55)", "kafka"),
            "compd": (78, 14, 16, 7, "COMPENSATED\n(AS-IS 70)", "client"),
            "compf": (78, 2, 16, 7, "COMPENSATION_FAILED\n(AS-IS 80 / 85)\n-> manual / TMF642", "legacy"),
            "skip": (20, 24, 13, 7, "SKIPPED /\nCANCELLED", "ne"),
        },
        [
            ("planned", "ready", "deps met"),
            ("ready", "disp", "outbox"),
            ("disp", "inflight", "adapter ack"),
            ("inflight", "ok", "2xx / OK"),
            ("inflight", "retry", "timeout / 5xx / 429"),
            ("retry", "disp", "re-dispatch"),
            ("inflight", "fail", "4xx / non-retryable\n/ attempts exhausted"),
            ("fail", "comp", "saga rollback"),
            ("comp", "compd"),
            ("comp", "compf"),
            ("planned", "skip", "cancel / superseded"),
        ],
        size=(13, 6),
        title="NE task state machine (with AS-IS status-code mapping)",
    )

    # ---------------- ER ----------------
    paths["er"] = box_diagram(
        "07_er",
        {
            "ten": (2, 62, 18, 10, "tenant\nPK (tenant, id)\nname, type (HOST|FULL_MVNO|\nLIGHT_MVNO), sla_tier,\nquota_tps, status", "obs"),
            "acc": (74, 46, 18, 10, "tenant_ne_access (M:N)\nPK (tenant, id)\ntenant -> shared NE,\nquota_tps, reserved_pct", "obs"),
            "ord": (2, 46, 18, 10, "service_order\nPK (tenant, id)\nexternal_id, state,\npriority, channel,\nrequested_at", "db"),
            "bc": (26, 46, 18, 10, "business_command\nPK (tenant, id)\norder_id, entity_key,\nentity_seq, spec_id,\nstate, params jsonb", "db"),
            "ent": (50, 46, 18, 10, "entity_sequencer\nPK (tenant, id)\nUQ (tenant, entity_key)\nnext_seq, active_bc_id", "db"),
            "link": (26, 28, 18, 10, "bc_task_link (M:N)\nPK (tenant, id)\nbc_id, task_id,\ncontribution jsonb", "db"),
            "task": (50, 28, 18, 10, "ne_task\nPK (tenant, id)\norder_id, ne_id, operation,\nstate, attempt,\nidempotency_key", "db"),
            "dep": (74, 28, 18, 10, "task_dependency (DAG)\nPK (tenant, id)\ntask_id, depends_on_id,\nkind (hard/soft)", "db"),
            "ne": (50, 10, 18, 10, "network_element\nPK (tenant, id)\nowner tenant, domain,\nvendor, nf_type,\nmax_tps, shared flag", "db"),
            "ep": (74, 10, 18, 10, "ne_endpoint (M:N)\nPK (tenant, id)\nne_id, adapter_type,\nbase_uri, auth_ref,\nweight, health", "db"),
            "spec": (2, 28, 18, 10, "command_spec\nPK (tenant, id)\n(tenant or GLOBAL)\nTMF633/634, versioned", "db"),
            "out": (2, 10, 18, 10, "outbox / inbox\nPK (tenant, id)\ntopic, key, payload,\npublished_at / consumer", "db"),
            "att": (26, 10, 18, 10, "task_attempt\nPK (tenant, id)\ntask_id, n, started,\nlatency_us, ne_status", "db"),
        },
        [
            ("ten", "ord", "1:N"), ("ord", "bc", "1:N"), ("bc", "ent", "N:1"), ("bc", "link", "1:N"), ("task", "link", "1:N"),
            ("task", "dep", "1:N"), ("task", "ne", "N:1"), ("ne", "ep", "1:N"), ("spec", "bc"),
            ("task", "att", "1:N"),
        ],
        size=(13, 8.5),
        ylim=(0, 74),
        title="DxPS PostgreSQL logical data model - every table: tenant varchar(40) + id uuid composite PK",
    )
    return paths


if __name__ == "__main__":
    for k, v in build_all().items():
        print(k, v)
