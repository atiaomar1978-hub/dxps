#!/usr/bin/env bash
# Installs the DxPS prerequisites on Ubuntu 22.04 / 24.04 (amd64 or arm64):
#   PostgreSQL 18 (PGDG apt repository), OpenJDK 21 (for Kafka), Go (official tarball, SHA-256 verified),
#   Apache Kafka 4 (Apache release, SHA-512 verified, into the DxPS runtime directory).
# Needs sudo for apt. Re-running is safe.
source "$(dirname "$0")/env.sh"

[ "$DXPS_OS" = "Linux" ] || die "this script is for Ubuntu; use install-deps-macos.sh on macOS"
SUDO=""; [ "$(id -u)" -eq 0 ] || SUDO="sudo"

log "== apt: base tools and OpenJDK 21"
$SUDO apt-get update -y
$SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
  ca-certificates curl gnupg lsb-release tar gzip openjdk-21-jre-headless

log "== PostgreSQL $PG_MAJOR from apt.postgresql.org"
if [ ! -x "/usr/lib/postgresql/$PG_MAJOR/bin/initdb" ]; then
  $SUDO install -d /usr/share/postgresql-common/pgdg
  $SUDO curl -fsSL -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc
  echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" |
    $SUDO tee /etc/apt/sources.list.d/pgdg.list >/dev/null
  # DxPS runs its own cluster under the runtime directory (port 5433); do not create the system cluster.
  $SUDO install -d /etc/postgresql-common
  echo "create_main_cluster = false" | $SUDO tee /etc/postgresql-common/createcluster.conf >/dev/null
  $SUDO apt-get update -y
  $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "postgresql-$PG_MAJOR"
fi
"/usr/lib/postgresql/$PG_MAJOR/bin/postgres" --version

log "== Go $GO_VERSION"
have_go() { command -v go >/dev/null 2>&1 && go version | grep -Eq "go1\.(2[6-9]|[3-9][0-9])"; }
if ! have_go; then
  case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64 | arm64) arch=arm64 ;; *) die "unsupported CPU $(uname -m)" ;; esac
  f="go$GO_VERSION.linux-$arch.tar.gz"
  mkdir -p "$RT/dl" "$RT/tools"
  curl -fSL --progress-bar -o "$RT/dl/$f" "https://dl.google.com/go/$f"
  want="$(curl -fsSL "https://dl.google.com/go/$f.sha256" | tr -d ' \r\n')"
  [ "$want" = "$(sha256_of "$RT/dl/$f")" ] || die "Go download checksum mismatch"
  rm -rf "$RT/tools/go"; tar -xzf "$RT/dl/$f" -C "$RT/tools"
  export PATH="$RT/tools/go/bin:$PATH"
  log "go: installed into $RT/tools/go (scripts add it to PATH automatically)"
fi
go version

log "== Apache Kafka $KAFKA_VERSION"
install_kafka
java -version 2>&1 | head -n 1
log "prerequisites ready - next: scripts/infra-setup.sh"
