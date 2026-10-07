#!/usr/bin/env bash
# Installs the DxPS prerequisites on macOS 13+ (Apple silicon or Intel) with Homebrew:
#   Go, PostgreSQL 18, OpenJDK 21 (for Kafka), and Apache Kafka 4 (Apache release, SHA-512 verified,
#   into the DxPS runtime directory). Homebrew services are not started: DxPS runs its own PostgreSQL
#   cluster on port 5433 and its own Kafka broker. Re-running is safe.
source "$(dirname "$0")/env.sh"

[ "$DXPS_OS" = "Darwin" ] || die "this script is for macOS; use install-deps-ubuntu.sh on Ubuntu"
command -v brew >/dev/null 2>&1 || die "Homebrew is required: https://brew.sh"

log "== Homebrew: go, postgresql@$PG_MAJOR, openjdk@21"
brew install go "postgresql@$PG_MAJOR" openjdk@21
PGBIN="$(brew --prefix "postgresql@$PG_MAJOR")/bin"
export JAVA_HOME="$(brew --prefix openjdk@21)/libexec/openjdk.jdk/Contents/Home"
export PATH="$JAVA_HOME/bin:$PATH"
"$PGBIN/postgres" --version
go version | grep -Eq "go1\.(2[6-9]|[3-9][0-9])" || log "note: go.mod pins toolchain go$GO_VERSION; Go downloads it automatically"
go version

log "== Apache Kafka $KAFKA_VERSION"
install_kafka
java -version 2>&1 | head -n 1
log "prerequisites ready - next: scripts/infra-setup.sh"
