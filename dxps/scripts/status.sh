#!/usr/bin/env bash
# Show which DxPS components are listening.
source "$(dirname "$0")/env.sh"
set +e
for c in "postgres 5433" "kafka 9092" "netsim 9199" "adapter 9202" "orchestrator 9201" "eventhub 9203" "gateway 8443" "dashboard 8088"; do
  set -- $c
  if test_port "$2"; then st=up; else st=down; fi
  printf '%-13s %-5s port %s\n' "$1" "$st" "$2"
done
