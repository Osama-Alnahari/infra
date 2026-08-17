#!/usr/bin/env bash
set -euo pipefail

readonly config=/opt/nomad/config/default.hcl

if [[ "$(grep -F 'node_pool = "canary"' "$config" | wc -l)" -ne 1 ]]; then
  echo "refusing promotion: expected one canary client node_pool" >&2
  exit 1
fi

sed -i \
  -e 's/node_pool = "canary"/node_pool = "default"/' \
  -e 's/"node_pool" = "canary"/"node_pool" = "default"/' \
  "$config"

supervisorctl restart nomad
echo '{"event":"stateless_worker_promoted","node_pool":"default"}'
