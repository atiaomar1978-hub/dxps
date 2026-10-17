#!/usr/bin/env bash
# Stops the DxPS console API and web UI started by console-run.sh.
source "$(dirname "$0")/env.sh"

for s in console-ui console; do
  f="$RUN/$s.pid"
  [ -f "$f" ] || continue
  pid="$(cat "$f")"
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
    for _ in 1 2 3 4 5 6 7 8 9 10; do kill -0 "$pid" 2>/dev/null || break; sleep 0.3; done
    kill -9 "$pid" 2>/dev/null || true
    log "$s stopped"
  fi
  rm -f "$f"
done
