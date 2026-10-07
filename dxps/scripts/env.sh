# Shared settings for the DxPS local runtime scripts on Ubuntu and macOS (source this file).
# Compatible with bash 3.2 (macOS default) and later.
set -euo pipefail

DXPS_OS="$(uname -s)"            # Linux | Darwin
SCRIPTS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="$(cd "$SCRIPTS/.." && pwd)"
RT="${DXPS_RUNTIME:-$HOME/dxps-runtime}"
BIN="$RT/bin"
PGDATA="$RT/data/pg"
KAFKA="$RT/kafka"
KCONF="$RT/kafka-config/server.properties"
KDATA="$RT/data/kafka"
LOGS="$RT/logs"
RUN="$RT/run"
export DXPS_RUNTIME="$RT"

KAFKA_VERSION="${DXPS_KAFKA_VERSION:-4.3.1}"
KAFKA_SCALA="2.13"
PG_MAJOR="${DXPS_PG_MAJOR:-18}"
GO_VERSION="${DXPS_GO_VERSION:-1.26.6}"
SERVICES="netsim adapter orchestrator eventhub gateway dashboard"

# Go installed by install-deps-ubuntu.sh into the runtime takes precedence.
if [ -x "$RT/tools/go/bin/go" ]; then export PATH="$RT/tools/go/bin:$PATH"; fi
export PATH="$(go env GOPATH 2>/dev/null || echo "$HOME/go")/bin:$PATH"

# PostgreSQL binaries: PGDG packages on Ubuntu, Homebrew on macOS (override with DXPS_PGBIN).
if [ -n "${DXPS_PGBIN:-}" ]; then
  PGBIN="$DXPS_PGBIN"
elif [ "$DXPS_OS" = "Darwin" ] && command -v brew >/dev/null 2>&1; then
  PGBIN="$(brew --prefix "postgresql@$PG_MAJOR" 2>/dev/null)/bin"
else
  PGBIN="/usr/lib/postgresql/$PG_MAJOR/bin"
fi

# Java 17+ for Kafka 4 (Homebrew keg-only openjdk on macOS).
if [ -z "${JAVA_HOME:-}" ] && [ "$DXPS_OS" = "Darwin" ] && command -v brew >/dev/null 2>&1; then
  jh="$(brew --prefix openjdk@21 2>/dev/null)/libexec/openjdk.jdk/Contents/Home"
  if [ -d "$jh" ]; then export JAVA_HOME="$jh"; export PATH="$JAVA_HOME/bin:$PATH"; fi
fi

log() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

test_port() { (exec 3<>"/dev/tcp/127.0.0.1/$1") >/dev/null 2>&1; }

wait_port() { # port [seconds]
  local port="$1" secs="${2:-90}" i=0
  while [ "$i" -lt $((secs * 2)) ]; do
    if test_port "$port"; then return 0; fi
    sleep 0.5; i=$((i + 1))
  done
  die "port $port did not open within $secs s"
}

# read_secret KEY - value from secrets.env (never echoed by the scripts).
read_secret() { sed -n "s/^$1=//p" "$RT/secrets.env" | head -n 1; }

# psql_as_super DB < sql   - runs SQL as postgres over TLS; the password stays in this process's environment.
psql_as_super() {
  PGPASSWORD="$(read_secret PG_SUPER_PASSWORD)" PGSSLMODE=verify-full PGSSLROOTCERT="$RT/pki/ca.crt" \
    "$PGBIN/psql" -X -q -v ON_ERROR_STOP=1 -h localhost -p 5433 -U postgres -d "$1" -f -
}

sha512_of() { if command -v sha512sum >/dev/null 2>&1; then sha512sum "$1" | cut -d' ' -f1; else shasum -a 512 "$1" | cut -d' ' -f1; fi; }
sha256_of() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

# install_kafka - downloads the Apache Kafka binary release into $RT/kafka and verifies its SHA-512.
install_kafka() {
  if [ -x "$KAFKA/bin/kafka-server-start.sh" ]; then log "kafka: $KAFKA already installed"; return 0; fi
  local name="kafka_${KAFKA_SCALA}-${KAFKA_VERSION}" dl="$RT/dl" url=""
  mkdir -p "$dl"
  for base in "https://downloads.apache.org/kafka" "https://archive.apache.org/dist/kafka"; do
    if curl -fsSL -o "$dl/$name.tgz.sha512" "$base/$KAFKA_VERSION/$name.tgz.sha512"; then url="$base/$KAFKA_VERSION/$name.tgz"; break; fi
  done
  [ -n "$url" ] || die "Kafka $KAFKA_VERSION not found on the Apache mirrors"
  log "kafka: downloading $url"
  curl -fSL --progress-bar -o "$dl/$name.tgz" "$url"
  # Apache publishes "file: HEX HEX ..." (gpg --print-md); normalise before comparing.
  local want got
  want="$(sed 's/^[^:]*://' "$dl/$name.tgz.sha512" | tr -d ' \t\r\n' | tr 'A-F' 'a-f')"
  got="$(sha512_of "$dl/$name.tgz")"
  [ "$want" = "$got" ] || die "Kafka download checksum mismatch"
  tar -xzf "$dl/$name.tgz" -C "$RT"
  rm -rf "$KAFKA"; mv "$RT/$name" "$KAFKA"
  log "kafka: installed $KAFKA_VERSION (sha512 verified)"
}

start_pg() {
  if test_port 5433; then log "postgres: already running"; return 0; fi
  mkdir -p "$LOGS"
  "$PGBIN/pg_ctl" -D "$PGDATA" -l "$LOGS/postgres.log" -w start >/dev/null
  log "postgres: started on localhost:5433 (TLS)"
}

start_kafka() {
  if test_port 9092; then log "kafka: already running"; return 0; fi
  mkdir -p "$LOGS/kafka"
  LOG_DIR="$LOGS/kafka" KAFKA_HEAP_OPTS="${KAFKA_HEAP_OPTS:--Xms512m -Xmx1g}" \
    "$KAFKA/bin/kafka-server-start.sh" -daemon "$KCONF"
  wait_port 9092 120
  log "kafka: started on 127.0.0.1:9092"
}

stop_services() {
  local s pid pids="" i
  for s in $SERVICES; do
    if [ -f "$RUN/$s.pid" ]; then
      pid="$(cat "$RUN/$s.pid")"
      if kill -0 "$pid" 2>/dev/null; then kill "$pid" 2>/dev/null || true; pids="$pids $pid"; fi
      rm -f "$RUN/$s.pid"
    fi
  done
  # graceful shutdown first (SIGTERM drains consumers), SIGKILL after 15 s
  for i in $(seq 1 15); do
    s=""
    for pid in $pids; do kill -0 "$pid" 2>/dev/null && s="$s $pid"; done
    [ -z "$s" ] && return 0
    sleep 1
  done
  for pid in $s; do kill -9 "$pid" 2>/dev/null || true; done
}

port_of() {
  case "$1" in
    netsim) echo 9199 ;; adapter) echo 9202 ;; orchestrator) echo 9201 ;;
    eventhub) echo 9203 ;; gateway) echo 8443 ;; dashboard) echo 8088 ;;
  esac
}
