-- DxPS core schema (LLD 6.11). Every table: tenant dxps_tenant first, id uuid (UUIDv7),
-- PRIMARY KEY (tenant, id), composite FKs, tenant-prefixed indexes, RLS enabled and forced.
SET search_path = dxps;

CREATE DOMAIN dxps_tenant AS varchar(40)
  CHECK (VALUE ~ '^[A-Za-z0-9_.-]{1,40}$');

-- Current tenant of the transaction (set_config('app.tenant', $1, true)); NULL when unset => no rows.
CREATE FUNCTION current_tenant() RETURNS dxps_tenant
  LANGUAGE sql STABLE PARALLEL SAFE
  AS $$ SELECT NULLIF(current_setting('app.tenant', true), '')::dxps_tenant $$;

-- Lowest UUIDv7 for a timestamp (RANGE partition bound on time-ordered ids).
CREATE FUNCTION uuid_floor(ts timestamptz) RETURNS uuid
  LANGUAGE sql IMMUTABLE PARALLEL SAFE
  AS $$ SELECT (lpad(to_hex(floor(extract(epoch FROM ts) * 1000)::bigint), 12, '0') || '70008000000000000000')::uuid $$;

-- ---------------------------------------------------------------- tenancy
CREATE TABLE tenant (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
  name text NOT NULL, tenant_type text NOT NULL CHECK (tenant_type IN ('HOST','FULL_MVNO','LIGHT_MVNO','ENTERPRISE')),
  host_tenant dxps_tenant,
  sla_tier text NOT NULL, sla_weight smallint NOT NULL CHECK (sla_weight BETWEEN 1 AND 64),
  quota_tps int NOT NULL CHECK (quota_tps > 0),
  status text NOT NULL CHECK (status IN ('ACTIVE','SUSPENDED','OFFBOARDING')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, id), UNIQUE (tenant)
);

CREATE TABLE network_element (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
  code text NOT NULL, domain text NOT NULL, vendor text, nf_type text NOT NULL, release text,
  shared bool NOT NULL DEFAULT false, max_tps int NOT NULL, max_concurrency int NOT NULL,
  reserved_pct smallint NOT NULL DEFAULT 20, attrs jsonb,
  PRIMARY KEY (tenant, id), UNIQUE (tenant, code)
);

CREATE TABLE ne_endpoint (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
  ne_id uuid NOT NULL, adapter_type text NOT NULL, base_uri text NOT NULL CHECK (base_uri LIKE 'https://%'),
  auth_ref text NOT NULL, weight int NOT NULL DEFAULT 100, site text,
  PRIMARY KEY (tenant, id), UNIQUE (tenant, ne_id, adapter_type, base_uri),
  FOREIGN KEY (tenant, ne_id) REFERENCES network_element (tenant, id)
);

CREATE TABLE tenant_ne_access (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
  ne_tenant dxps_tenant NOT NULL, ne_id uuid NOT NULL,
  quota_tps int NOT NULL, reserved_pct smallint NOT NULL DEFAULT 10,
  allowed_ops text[] NOT NULL, subscriber_group text,
  PRIMARY KEY (tenant, id), UNIQUE (tenant, ne_tenant, ne_id),
  FOREIGN KEY (ne_tenant, ne_id) REFERENCES network_element (tenant, id)
);

CREATE TABLE command_spec (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
  spec_code text NOT NULL, version text NOT NULL, status text NOT NULL, body jsonb NOT NULL,
  PRIMARY KEY (tenant, id), UNIQUE (tenant, spec_code, version)
);

-- ---------------------------------------------------------------- orders (LIST-partitioned by tenant)
CREATE TABLE service_order (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
  external_id text, channel text NOT NULL, category text,
  state text NOT NULL, priority smallint NOT NULL CHECK (priority BETWEEN 0 AND 3), tx_mode text NOT NULL,
  idempotency_key text NOT NULL, request_hash bytea NOT NULL, items jsonb NOT NULL,
  requested_at timestamptz NOT NULL, completed_at timestamptz, updated_at timestamptz NOT NULL DEFAULT now(),
  error_code text, error text, version int NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant, id), UNIQUE (tenant, external_id), UNIQUE (tenant, idempotency_key)
) PARTITION BY LIST (tenant);
CREATE INDEX ON service_order (tenant, state, requested_at DESC);

CREATE TABLE business_command (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
  order_id uuid NOT NULL, order_item_id text NOT NULL, message_id uuid NOT NULL,
  entity_key text NOT NULL, entity_keys text[] NOT NULL, entity_seq bigint NOT NULL,
  spec_id uuid NOT NULL, spec_tenant dxps_tenant NOT NULL, spec_code text NOT NULL, spec_version text NOT NULL,
  action text NOT NULL, state text NOT NULL, priority smallint NOT NULL, tx_mode text NOT NULL,
  params jsonb NOT NULL, depends_on_items text[] NOT NULL DEFAULT '{}', preempt bool NOT NULL DEFAULT false,
  error_code text, error text,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, id), UNIQUE (tenant, order_id, order_item_id),
  FOREIGN KEY (tenant, order_id) REFERENCES service_order (tenant, id),
  FOREIGN KEY (spec_tenant, spec_id) REFERENCES command_spec (tenant, id)
) PARTITION BY LIST (tenant);
CREATE INDEX ON business_command (tenant, order_id);
CREATE INDEX ON business_command (tenant, entity_key, entity_seq) WHERE state = 'pending';

CREATE TABLE bc_entity (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
  bc_id uuid NOT NULL, entity_key text NOT NULL,
  PRIMARY KEY (tenant, id), UNIQUE (tenant, entity_key, bc_id),
  FOREIGN KEY (tenant, bc_id) REFERENCES business_command (tenant, id)
) PARTITION BY LIST (tenant);

CREATE TABLE entity_sequencer (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
  entity_key text NOT NULL, next_seq bigint NOT NULL DEFAULT 1,
  active_bc_id uuid, updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, id), UNIQUE (tenant, entity_key)
) PARTITION BY LIST (tenant);

CREATE TABLE ne_task (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
  order_id uuid NOT NULL,
  ne_tenant dxps_tenant NOT NULL, ne_id uuid NOT NULL, domain text NOT NULL,
  operation text NOT NULL, state text NOT NULL, attempt int NOT NULL DEFAULT 0, in_degree int NOT NULL,
  priority smallint NOT NULL, seq int NOT NULL DEFAULT 0,
  is_compensation bool NOT NULL DEFAULT false, compensates_id uuid, after_pivot bool NOT NULL DEFAULT false,
  idempotency_key text NOT NULL, coalesce_key text, plan jsonb NOT NULL, request jsonb, response jsonb,
  ne_status int, ne_code text, last_error text,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  dispatched_at timestamptz, completed_at timestamptz,
  PRIMARY KEY (tenant, id),
  UNIQUE (tenant, idempotency_key),
  FOREIGN KEY (tenant, order_id) REFERENCES service_order (tenant, id),
  FOREIGN KEY (ne_tenant, ne_id) REFERENCES network_element (tenant, id)
) PARTITION BY LIST (tenant);
CREATE INDEX ON ne_task (tenant, order_id);
CREATE INDEX ON ne_task (tenant, order_id, coalesce_key) WHERE coalesce_key IS NOT NULL;

CREATE TABLE bc_task_link (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
  order_id uuid NOT NULL, bc_id uuid NOT NULL, task_id uuid NOT NULL, contribution jsonb,
  PRIMARY KEY (tenant, id), UNIQUE (tenant, bc_id, task_id),
  FOREIGN KEY (tenant, bc_id) REFERENCES business_command (tenant, id),
  FOREIGN KEY (tenant, task_id) REFERENCES ne_task (tenant, id)
) PARTITION BY LIST (tenant);
CREATE INDEX ON bc_task_link (tenant, task_id);

CREATE TABLE task_dependency (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
  order_id uuid NOT NULL, task_id uuid NOT NULL, depends_on_id uuid NOT NULL,
  kind text NOT NULL DEFAULT 'hard',
  PRIMARY KEY (tenant, id), UNIQUE (tenant, depends_on_id, task_id),
  FOREIGN KEY (tenant, task_id) REFERENCES ne_task (tenant, id),
  FOREIGN KEY (tenant, depends_on_id) REFERENCES ne_task (tenant, id)
) PARTITION BY LIST (tenant);
CREATE INDEX ON task_dependency (tenant, task_id);

-- ---------------------------------------------------------------- append-only (RANGE on UUIDv7 id = time)
CREATE TABLE task_attempt (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
  task_id uuid NOT NULL, n int NOT NULL, started_at timestamptz NOT NULL,
  latency_us bigint, ne_status int, ne_code text, outcome text, error text,
  PRIMARY KEY (tenant, id)
) PARTITION BY RANGE (id);
CREATE INDEX ON task_attempt (tenant, task_id);

CREATE TABLE audit_event (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
  actor text NOT NULL, acting_for_tenant bool NOT NULL DEFAULT false,
  action text NOT NULL, object_type text NOT NULL, object_id uuid, detail jsonb,
  PRIMARY KEY (tenant, id)
) PARTITION BY RANGE (id);

-- ---------------------------------------------------------------- messaging
CREATE TABLE outbox (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
  shard smallint NOT NULL,
  topic text NOT NULL, key bytea NOT NULL, payload bytea NOT NULL, headers jsonb,
  created_at timestamptz NOT NULL DEFAULT now(), published_at timestamptz,
  PRIMARY KEY (tenant, id)
);
CREATE INDEX outbox_unpublished ON outbox (shard, id) WHERE published_at IS NULL;

CREATE TABLE orchestrator_inbox (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL,
  received_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, id)
) PARTITION BY RANGE (id);

CREATE TABLE hub_subscription (
  tenant dxps_tenant NOT NULL, id uuid NOT NULL DEFAULT uuidv7(),
  callback text NOT NULL CHECK (callback LIKE 'https://%'), query text, secret_ref text NOT NULL, status text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, id)
);

-- ---------------------------------------------------------------- partitions
-- LIST(tenant): dedicated partitions for large tenants, all others in a HASH-partitioned default.
CREATE PROCEDURE create_tenant_partitions(tbl text, ff int, dedicated text[], hash_parts int)
LANGUAGE plpgsql AS $$
DECLARE t text; i int;
BEGIN
  FOREACH t IN ARRAY dedicated LOOP
    EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES IN (%L) WITH (fillfactor = %s)',
                   tbl || '_' || replace(replace(t, '-', '_'), '.', '_'), tbl, t, ff);
  END LOOP;
  EXECUTE format('CREATE TABLE %I PARTITION OF %I DEFAULT PARTITION BY HASH (tenant)', tbl || '_shared', tbl);
  FOR i IN 0 .. hash_parts - 1 LOOP
    EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES WITH (MODULUS %s, REMAINDER %s) WITH (fillfactor = %s)',
                   tbl || '_shared_' || lpad(i::text, 2, '0'), tbl || '_shared', hash_parts, i, ff);
  END LOOP;
END $$;

CALL create_tenant_partitions('service_order',    90, ARRAY['host-mno','mvno-alpha'], 4);
CALL create_tenant_partitions('business_command', 90, ARRAY['host-mno','mvno-alpha'], 4);
CALL create_tenant_partitions('bc_entity',       100, ARRAY['host-mno','mvno-alpha'], 4);
CALL create_tenant_partitions('entity_sequencer', 70, ARRAY['host-mno','mvno-alpha'], 4);
CALL create_tenant_partitions('ne_task',          80, ARRAY['host-mno','mvno-alpha'], 4);
CALL create_tenant_partitions('bc_task_link',    100, ARRAY['host-mno','mvno-alpha'], 4);
CALL create_tenant_partitions('task_dependency', 100, ARRAY['host-mno','mvno-alpha'], 4);

-- RANGE(id): daily partitions bounded by the UUIDv7 of the day start, plus a default safety net.
CREATE PROCEDURE ensure_daily_partitions(tbl text, days_back int, days_ahead int)
LANGUAGE plpgsql AS $$
DECLARE d date; p text;
BEGIN
  FOR d IN SELECT generate_series(current_date - days_back, current_date + days_ahead, '1 day')::date LOOP
    p := tbl || '_' || to_char(d, 'YYYYMMDD');
    IF to_regclass(p) IS NULL THEN
      EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)', p, tbl,
                     uuid_floor(d::timestamptz), uuid_floor((d + 1)::timestamptz));
    END IF;
  END LOOP;
  IF to_regclass(tbl || '_default') IS NULL THEN
    EXECUTE format('CREATE TABLE %I PARTITION OF %I DEFAULT', tbl || '_default', tbl);
  END IF;
END $$;

CALL ensure_daily_partitions('task_attempt', 1, 7);
CALL ensure_daily_partitions('audit_event', 1, 7);
CALL ensure_daily_partitions('orchestrator_inbox', 1, 7);

-- ---------------------------------------------------------------- row-level security
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['tenant','network_element','ne_endpoint','tenant_ne_access','service_order',
      'business_command','bc_entity','entity_sequencer','ne_task','bc_task_link','task_dependency',
      'task_attempt','audit_event','outbox','orchestrator_inbox','hub_subscription'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY tenant_isolation ON %I USING (tenant = current_tenant()) WITH CHECK (tenant = current_tenant())', t);
  END LOOP;
END $$;

ALTER TABLE command_spec ENABLE ROW LEVEL SECURITY;
ALTER TABLE command_spec FORCE ROW LEVEL SECURITY;
CREATE POLICY catalog_read ON command_spec FOR SELECT
  USING (tenant IN (current_tenant(), 'GLOBAL'));
CREATE POLICY catalog_write ON command_spec FOR ALL
  USING (tenant = current_tenant()) WITH CHECK (tenant = current_tenant());

-- ---------------------------------------------------------------- privileges
REVOKE ALL ON ALL TABLES IN SCHEMA dxps FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA dxps TO dxps_app;
GRANT DELETE ON hub_subscription TO dxps_app;
REVOKE UPDATE ON audit_event, task_attempt, orchestrator_inbox FROM dxps_app;   -- append-only
GRANT SELECT ON ALL TABLES IN SCHEMA dxps TO dxps_ops;
GRANT INSERT, UPDATE ON tenant, network_element, ne_endpoint, tenant_ne_access, command_spec TO dxps_ops;
GRANT UPDATE (published_at) ON outbox TO dxps_ops;
GRANT INSERT ON audit_event TO dxps_ops;
GRANT EXECUTE ON FUNCTION current_tenant(), uuid_floor(timestamptz) TO dxps_app, dxps_ops;

-- Migration bookkeeping follows the same tenant rule (rows belong to GLOBAL).
CREATE TABLE schema_migration (
  tenant dxps_tenant NOT NULL DEFAULT 'GLOBAL', id uuid NOT NULL DEFAULT uuidv7(),
  version text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, id), UNIQUE (tenant, version)
);
