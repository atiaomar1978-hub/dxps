#!/usr/bin/env bash
# Builds and starts the DxPS console: the API (https://localhost:8446) and the web UI (https://localhost:8092).
# Needs the platform running (run-all.sh). Usage: console-run.sh [--foreground]
#   --foreground  stay attached and stop both processes on exit (useful under a supervisor)
source "$(dirname "$0")/env.sh"

FG=0
[ "${1:-}" = "--foreground" ] && FG=1
UI="$SRC/console"
API_PORT="${DXPS_CONSOLE_API_PORT:-8446}"
UI_PORT="${DXPS_CONSOLE_PORT:-8092}"

if [ -d /opt/homebrew/opt/node@22/bin ]; then export PATH="/opt/homebrew/opt/node@22/bin:$PATH"; fi
command -v node >/dev/null 2>&1 || die "Node.js 22.12+ is required for the console UI"

mkdir -p "$BIN" "$RUN" "$LOGS/svc"
"$SCRIPTS/console-stop.sh" >/dev/null 2>&1 || true

log "Building console API"
(cd "$SRC" && go build -trimpath -o "$BIN/console" ./cmd/console)

log "Building console UI"
(cd "$UI" && { [ -d node_modules ] || npm ci --no-audit --no-fund; } && NEXT_TELEMETRY_DISABLED=1 npx next build >"$LOGS/svc/console-ui-build.out" 2>&1) ||
  die "UI build failed, see $LOGS/svc/console-ui-build.out"

export DXPS_LOG_DIR="$LOGS/svc"
export DXPS_CATALOG_DIR="${DXPS_CATALOG_DIR:-$SRC/internal/catalog/data}"
(cd "$RT" && exec nohup "$BIN/console" -addr "127.0.0.1:$API_PORT" >>"$LOGS/svc/console.out" 2>&1) &
echo $! >"$RUN/console.pid"
wait_port "$API_PORT" 60

# The API issues the console certificate from the platform CA on first start.
export DXPS_CONSOLE_CERT="$RT/pki/console.crt" DXPS_CONSOLE_KEY="$RT/pki/console.key"
export DXPS_CONSOLE_API_URL="https://localhost:$API_PORT" DXPS_CONSOLE_API_CA="$RT/pki/ca.crt"
export DXPS_CONSOLE_PORT="$UI_PORT" NODE_ENV=production NEXT_TELEMETRY_DISABLED=1
(cd "$UI" && exec nohup node server.mjs >>"$LOGS/svc/console-ui.out" 2>&1) &
echo $! >"$RUN/console-ui.pid"
wait_port "$UI_PORT" 60

log "Console API  https://localhost:$API_PORT (pid $(cat "$RUN/console.pid"))"
log "Console UI   https://localhost:$UI_PORT (pid $(cat "$RUN/console-ui.pid"))"
if [ ! -s "$RT/console/users.json" ]; then
  log "No console user yet. Create the first admin with:"
  log "  $BIN/console useradd -name admin -roles admin   (password read from stdin)"
fi

if [ "$FG" = 1 ]; then
  trap '"$SCRIPTS/console-stop.sh"' EXIT INT TERM
  wait
fi
