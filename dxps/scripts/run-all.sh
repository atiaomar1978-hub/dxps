#!/usr/bin/env bash
# Build and start the whole DxPS stack on Ubuntu / macOS: PostgreSQL, Kafka, simulators, adapters,
# orchestrator, event hub, gateway and the Mosaic dashboard (http://127.0.0.1:8088).
# usage: scripts/run-all.sh [--no-build]
source "$(dirname "$0")/env.sh"

build=1
[ "${1:-}" = "--no-build" ] && build=0
mkdir -p "$BIN" "$RUN" "$LOGS/svc"

start_pg
start_kafka

if [ "$build" = 1 ]; then
  (cd "$SRC" && for s in $SERVICES dxpsctl; do go build -trimpath -o "$BIN/$s" "./cmd/$s" || exit 1; done) || die "build failed"
  log "built: $SERVICES dxpsctl"
fi

"$BIN/dxpsctl" migrate >/dev/null
"$BIN/dxpsctl" topics >/dev/null
"$BIN/dxpsctl" seed >/dev/null

stop_services
export DXPS_LOG_DIR="$LOGS/svc"
for s in $SERVICES; do
  (cd "$RT" && exec nohup "$BIN/$s" >>"$LOGS/svc/$s.out" 2>&1) &
  echo $! >"$RUN/$s.pid"
  wait_port "$(port_of "$s")" 60
  printf '%-13s up on port %s (pid %s)\n' "$s" "$(port_of "$s")" "$(cat "$RUN/$s.pid")"
done
log ""
log "DxPS Mosaic:  http://127.0.0.1:8088"
log "TMF641 API:   https://localhost:8443/tmf-api/serviceOrdering/v5/serviceOrder"
log "Logs:         $LOGS/svc"
