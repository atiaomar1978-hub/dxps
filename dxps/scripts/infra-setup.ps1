# One-time setup of the local DxPS runtime: secrets, dev PKI, PostgreSQL 18 and Kafka 4 (KRaft).
. "$PSScriptRoot\env.ps1"

New-Item -ItemType Directory -Force $BIN, $LOGS | Out-Null
Push-Location $SRC
try { go build -o "$BIN\dxpsctl.exe" ./cmd/dxpsctl; if ($LASTEXITCODE) { throw 'build dxpsctl failed' } } finally { Pop-Location }
& "$BIN\dxpsctl.exe" secrets
& "$BIN\dxpsctl.exe" certs
$s = Read-Secrets

# ---------------------------------------------------------------- PostgreSQL 18
if (-not (Test-Path "$PGDATA\PG_VERSION")) {
    New-Item -ItemType Directory -Force $PGDATA | Out-Null
    $pw = Join-Path $RT 'pw.tmp'
    [IO.File]::WriteAllText($pw, $s['PG_SUPER_PASSWORD'])
    try {
        & "$PGBIN\initdb.exe" -D $PGDATA -U postgres -A scram-sha-256 --pwfile=$pw -E UTF8 --locale=C --data-checksums | Out-Null
        if ($LASTEXITCODE) { throw 'initdb failed' }
    } finally { Remove-Item $pw -Force }
    Copy-Item "$RT\pki\postgres.crt" "$PGDATA\server.crt"
    Copy-Item "$RT\pki\postgres.key" "$PGDATA\server.key"
    Copy-Item "$RT\pki\ca.crt" "$PGDATA\root.crt"
    Add-Content "$PGDATA\postgresql.conf" @"

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
"@
    # TLS-only, SCRAM-only, loopback-only. No trust/peer/md5 entries.
    Set-Content "$PGDATA\pg_hba.conf" @"
# TYPE   DATABASE  USER  ADDRESS        METHOD
hostssl  all       all   127.0.0.1/32   scram-sha-256
hostssl  all       all   ::1/128        scram-sha-256
"@
}
Start-Pg

$roles = @"
DO `$`$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'dxps_owner') THEN CREATE ROLE dxps_owner LOGIN NOBYPASSRLS; END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'dxps_app')   THEN CREATE ROLE dxps_app   LOGIN NOBYPASSRLS NOCREATEDB NOCREATEROLE; END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'dxps_ops')   THEN CREATE ROLE dxps_ops   LOGIN BYPASSRLS   NOCREATEDB NOCREATEROLE; END IF;
END `$`$;
ALTER ROLE dxps_owner PASSWORD '$($s['PG_OWNER_PASSWORD'])';
ALTER ROLE dxps_app   PASSWORD '$($s['PG_APP_PASSWORD'])' CONNECTION LIMIT 120;
ALTER ROLE dxps_ops   PASSWORD '$($s['PG_OPS_PASSWORD'])' CONNECTION LIMIT 30;
SELECT 'CREATE DATABASE dxps OWNER dxps_owner' WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'dxps') \gexec
REVOKE ALL ON DATABASE dxps FROM PUBLIC;
GRANT CONNECT ON DATABASE dxps TO dxps_app, dxps_ops;
"@
Invoke-Psql 'postgres' $roles
Invoke-Psql 'dxps' @"
REVOKE ALL ON SCHEMA public FROM PUBLIC;
CREATE SCHEMA IF NOT EXISTS dxps AUTHORIZATION dxps_owner;
GRANT USAGE ON SCHEMA dxps TO dxps_app, dxps_ops;
ALTER ROLE dxps_owner IN DATABASE dxps SET search_path = dxps;
ALTER ROLE dxps_app   IN DATABASE dxps SET search_path = dxps;
ALTER ROLE dxps_ops   IN DATABASE dxps SET search_path = dxps;
"@
Write-Host 'postgres: roles dxps_owner / dxps_app (RLS enforced) / dxps_ops (BYPASSRLS), database dxps'

# ---------------------------------------------------------------- Kafka 4 KRaft (combined broker + controller)
if (-not (Test-Path "$KDATA\meta.properties")) {
    New-Item -ItemType Directory -Force (Split-Path $KCONF), $KDATA | Out-Null
    $kd = $KDATA -replace '\\', '/'
    Set-Content $KCONF @"
process.roles=broker,controller
node.id=1
controller.quorum.bootstrap.servers=127.0.0.1:9093
listeners=PLAINTEXT://127.0.0.1:9092,CONTROLLER://127.0.0.1:9093
advertised.listeners=PLAINTEXT://127.0.0.1:9092,CONTROLLER://127.0.0.1:9093
controller.listener.names=CONTROLLER
inter.broker.listener.name=PLAINTEXT
listener.security.protocol.map=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT
log.dirs=$kd
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
# Windows dev runtime: segment rename/delete fails on open files (KAFKA-1194) and takes the log dir offline,
# so compaction and retention deletion are disabled. Production (Linux) keeps the defaults.
log.cleaner.enable=false
log.retention.check.interval.ms=9223372036854775807
"@
    $cid = (& "$KAFKA\bin\windows\kafka-storage.bat" random-uuid).Trim()
    & "$KAFKA\bin\windows\kafka-storage.bat" format --standalone -t $cid -c $KCONF | Out-Null
    if ($LASTEXITCODE) { throw 'kafka-storage format failed' }
}
Start-Kafka
Write-Host 'infra: ready'
