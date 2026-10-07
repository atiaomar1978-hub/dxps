#!/usr/bin/env bash
# One-time setup of the local DxPS runtime on Ubuntu / macOS: secrets, dev PKI, PostgreSQL 18 cluster
# (TLS 1.3, SCRAM, roles, database) and Kafka 4 (KRaft). Run install-deps-<os>.sh first. Re-running is safe.
source "$(dirname "$0")/env.sh"

[ -x "$PGBIN/initdb" ] || die "PostgreSQL $PG_MAJOR not found in $PGBIN (run install-deps first or set DXPS_PGBIN)"
[ -x "$KAFKA/bin/kafka-server-start.sh" ] || die "Kafka not found in $KAFKA (run install-deps first)"
command -v java >/dev/null 2>&1 || die "java not found (Kafka needs Java 17+)"
umask 077
mkdir -p "$BIN" "$LOGS" "$RUN"

log "== secrets and dev PKI"
(cd "$SRC" && go build -trimpath -o "$BIN/dxpsctl" ./cmd/dxpsctl)
"$BIN/dxpsctl" secrets
"$BIN/dxpsctl" certs

# ---------------------------------------------------------------- PostgreSQL
if [ ! -f "$PGDATA/PG_VERSION" ]; then
  log "== PostgreSQL: initdb $PGDATA"
  mkdir -p "$PGDATA"
  pw="$(mktemp "$RT/pw.XXXXXX")"
  trap 'rm -f "$pw"' EXIT
  read_secret PG_SUPER_PASSWORD >"$pw"
  "$PGBIN/initdb" -D "$PGDATA" -U postgres -A scram-sha-256 --pwfile="$pw" -E UTF8 --locale=C --data-checksums >/dev/null
  rm -f "$pw"; trap - EXIT
  install -m 600 "$RT/pki/postgres.crt" "$PGDATA/server.crt"
  install -m 600 "$RT/pki/postgres.key" "$PGDATA/server.key"
  install -m 600 "$RT/pki/ca.crt" "$PGDATA/root.crt"
  cat >>"$PGDATA/postgresql.conf" <<'EOF'

# ---- DxPS local settings
port = 5433
listen_addresses = 'localhost'
max_connections = 200
shared_buffers = 512MB
password_encryption = scram-sha-256
ssl = on
ssl_cert_file = 'server.crt'
ssl_key_file = 'server.key'
ssl_min_protocol_version = 'TLSv1.3'
wal_compression = lz4
log_connections = on
log_disconnections = on
log_line_prefix = '%m [%p] %u@%d %a '
EOF
  # TLS-only, SCRAM-only, loopback-only. No trust/peer/md5 entries.
  cat >"$PGDATA/pg_hba.conf" <<'EOF'
# TYPE   DATABASE  USER  ADDRESS        METHOD
hostssl  all       all   127.0.0.1/32   scram-sha-256
hostssl  all       all   ::1/128        scram-sha-256
EOF
fi
# TCP+TLS only. Debian/Ubuntu builds default the socket dir to /var/run/postgresql, owned by the
# system postgres user, which a per-user cluster cannot write to.
grep -q '^unix_socket_directories' "$PGDATA/postgresql.conf" ||
  printf "unix_socket_directories = ''\n" >>"$PGDATA/postgresql.conf"
start_pg

log "== PostgreSQL: roles and database"
psql_as_super postgres <<EOF
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'dxps_owner') THEN CREATE ROLE dxps_owner LOGIN NOBYPASSRLS; END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'dxps_app')   THEN CREATE ROLE dxps_app   LOGIN NOBYPASSRLS NOCREATEDB NOCREATEROLE; END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'dxps_ops')   THEN CREATE ROLE dxps_ops   LOGIN BYPASSRLS   NOCREATEDB NOCREATEROLE; END IF;
END \$\$;
ALTER ROLE dxps_owner PASSWORD '$(read_secret PG_OWNER_PASSWORD)';
ALTER ROLE dxps_app   PASSWORD '$(read_secret PG_APP_PASSWORD)' CONNECTION LIMIT 120;
ALTER ROLE dxps_ops   PASSWORD '$(read_secret PG_OPS_PASSWORD)' CONNECTION LIMIT 30;
SELECT 'CREATE DATABASE dxps OWNER dxps_owner' WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'dxps') \gexec
REVOKE ALL ON DATABASE dxps FROM PUBLIC;
GRANT CONNECT ON DATABASE dxps TO dxps_app, dxps_ops;
EOF
psql_as_super dxps <<'EOF'
REVOKE ALL ON SCHEMA public FROM PUBLIC;
CREATE SCHEMA IF NOT EXISTS dxps AUTHORIZATION dxps_owner;
GRANT USAGE ON SCHEMA dxps TO dxps_app, dxps_ops;
ALTER ROLE dxps_owner IN DATABASE dxps SET search_path = dxps;
ALTER ROLE dxps_app   IN DATABASE dxps SET search_path = dxps;
ALTER ROLE dxps_ops   IN DATABASE dxps SET search_path = dxps;
EOF
log "postgres: roles dxps_owner / dxps_app (RLS enforced) / dxps_ops (BYPASSRLS), database dxps"

# ---------------------------------------------------------------- Kafka 4 KRaft (combined broker + controller)
if [ ! -f "$KDATA/meta.properties" ]; then
  log "== Kafka: format KRaft storage"
  mkdir -p "$(dirname "$KCONF")" "$KDATA"
  cat >"$KCONF" <<EOF
process.roles=broker,controller
node.id=1
controller.quorum.bootstrap.servers=127.0.0.1:9093
listeners=PLAINTEXT://127.0.0.1:9092,CONTROLLER://127.0.0.1:9093
advertised.listeners=PLAINTEXT://127.0.0.1:9092,CONTROLLER://127.0.0.1:9093
controller.listener.names=CONTROLLER
inter.broker.listener.name=PLAINTEXT
listener.security.protocol.map=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT
log.dirs=$KDATA
num.partitions=6
auto.create.topics.enable=false
offsets.topic.replication.factor=1
transaction.state.log.replication.factor=1
transaction.state.log.min.isr=1
share.coordinator.state.topic.replication.factor=1
share.coordinator.state.topic.min.isr=1
group.coordinator.rebalance.protocols=classic,consumer
unclean.leader.election.enable=false
message.timestamp.type=CreateTime
log.segment.bytes=268435456
log.retention.hours=72
group.initial.rebalance.delay.ms=0
EOF
  cid="$("$KAFKA/bin/kafka-storage.sh" random-uuid)"
  "$KAFKA/bin/kafka-storage.sh" format --standalone -t "$cid" -c "$KCONF" >/dev/null
fi
start_kafka
log "infra: ready - next: scripts/run-all.sh"
