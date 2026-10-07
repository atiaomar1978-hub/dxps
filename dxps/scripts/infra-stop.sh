#!/usr/bin/env bash
# Stop the DxPS services, Kafka and PostgreSQL.
source "$(dirname "$0")/env.sh"
set +e
stop_services
log "services: stopped"
if test_port 9092; then "$KAFKA/bin/kafka-server-stop.sh" >/dev/null 2>&1; log "kafka: stopping"; fi
if [ -f "$PGDATA/postmaster.pid" ]; then "$PGBIN/pg_ctl" -D "$PGDATA" -m fast -w stop >/dev/null && log "postgres: stopped"; fi
