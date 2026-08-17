#!/usr/bin/env bash
set -euo pipefail

# Stateless production-worker bootstrap. This artifact started as the Phase-1
# canary wrapper; after capacity and cold-resume validation, stateless workers
# join the production `default` Nomad pool so the existing orchestrator system
# job and API discovery path can use them.

readonly BASE_SCRIPT_URI="gs://gen-lang-client-0975136333-etlaq-e2b-poc-instance-setup/start-client-stateless-canary-base-20260814.sh"
readonly BASE_SCRIPT="/var/lib/etlaq-e2b/start-client-base.sh"
readonly WORKER_SCRIPT="/var/lib/etlaq-e2b/start-client-stateless.sh"

install -d -m 0755 "$(dirname "$BASE_SCRIPT")"
gsutil cp "$BASE_SCRIPT_URI" "$BASE_SCRIPT"

# The preserved production bootstrap currently carries a UTF-8 BOM. Strip it
# from the private copy so the kernel reads the shebang correctly on every
# stateless recreation.
sed -i '1s/^\xEF\xBB\xBF//' "$BASE_SCRIPT"

# Fail closed if the pinned bootstrap no longer has the expected production
# node-pool argument.
if [[ "$(grep -F -- '--node-pool "default"' "$BASE_SCRIPT" | wc -l)" -ne 1 ]]; then
  echo "Stateless bootstrap refused: expected exactly one default node-pool argument" >&2
  exit 1
fi

install -m 0700 "$BASE_SCRIPT" "$WORKER_SCRIPT"

exec "$WORKER_SCRIPT"
