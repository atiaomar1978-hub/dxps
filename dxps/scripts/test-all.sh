#!/usr/bin/env bash
# Runs the full DxPS test suite (unit + integration against the local Kafka/PostgreSQL) and publishes
# <runtime>/results/tests.json for the Mosaic Tests tile, plus the raw go test -json stream and coverage profile.
# usage: scripts/test-all.sh [-run REGEX] [-p N]
source "$(dirname "$0")/env.sh"

run=""; par=2
while [ $# -gt 0 ]; do
  case "$1" in -run) run="$2"; shift 2 ;; -p) par="$2"; shift 2 ;; *) die "unknown option $1" ;; esac
done
out="$RT/results"; mkdir -p "$out"
raw="$out/tests-raw.jsonl"; prof="$out/coverage.out"

cd "$SRC"
args=(test -json -count=1 "-p=$par" -timeout=20m -covermode=set -coverpkg=./internal/... "-coverprofile=$prof")
[ -n "$run" ] && args+=("-run=$run")
t0=$(date +%s)
go "${args[@]}" ./... >"$raw" 2>/dev/null || true
t1=$(date +%s)
go run ./cmd/dxpsctl testreport -json "$raw" -cover "$prof" -out "$out/tests.json" -elapsed "$((t1 - t0))"
