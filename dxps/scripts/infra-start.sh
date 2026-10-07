#!/usr/bin/env bash
# Start PostgreSQL and Kafka (after infra-setup.sh has run once).
source "$(dirname "$0")/env.sh"
start_pg
start_kafka
