#!/usr/bin/env bash
# Static security scans: govulncheck (known CVEs reachable from DxPS code) and gosec (insecure patterns).
# Reports land in <runtime>/results. G404 is excluded: math/rand is used only for retry jitter, simulator latency
# and demo traffic mix; every secret (tokens, CSRF nonces, certificate serials, keys) uses crypto/rand.
source "$(dirname "$0")/env.sh"
out="$RT/results"; mkdir -p "$out"
command -v govulncheck >/dev/null 2>&1 || go install golang.org/x/vuln/cmd/govulncheck@latest
command -v gosec >/dev/null 2>&1 || go install github.com/securego/gosec/v2/cmd/gosec@latest
cd "$SRC"
rc=0
if govulncheck -show verbose ./... >"$out/govulncheck.txt" 2>&1; then log "govulncheck: no vulnerabilities found"; else log "govulncheck: findings - see $out/govulncheck.txt"; rc=1; fi
gosec -quiet -fmt json -out "$out/gosec.json" -exclude=G404 ./... >/dev/null 2>&1 || true
gosec -quiet -fmt text -severity medium -exclude=G404 ./... >"$out/gosec-medium.txt" 2>&1 && log "gosec: no medium/high findings" ||
  { log "gosec: medium/high findings - see $out/gosec-medium.txt"; rc=1; }
exit $rc
