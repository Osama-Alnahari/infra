#!/usr/bin/env bash
set -euo pipefail

readonly env_file=/etc/etlaq/e2b-stateless-scaler.env
readonly expected_old_mig=etlaq-e2b-poc-orch-client-stateless-canary-rig
readonly expected_old_prefix=etlaq-e2b-stateless-canary-
readonly new_mig=etlaq-e2b-poc-orch-client-stateless-rig
readonly new_prefix=etlaq-e2b-poc-orch-client-stateless-

current_mig="$(sed -n 's/^E2B_SCALER_MIG=//p' "$env_file")"
current_prefix="$(sed -n 's/^E2B_SCALER_WORKER_PREFIX=//p' "$env_file")"

if [[ "$current_mig" != "$expected_old_mig" && "$current_mig" != "$new_mig" ]]; then
  echo "unexpected current MIG" >&2
  exit 1
fi
if [[ "$current_prefix" != "$expected_old_prefix" && "$current_prefix" != "$new_prefix" ]]; then
  echo "unexpected current worker prefix" >&2
  exit 1
fi

sed -i \
  -e "s/^E2B_SCALER_MIG=.*/E2B_SCALER_MIG=${new_mig}/" \
  -e "s/^E2B_SCALER_WORKER_PREFIX=.*/E2B_SCALER_WORKER_PREFIX=${new_prefix}/" \
  -e 's/^E2B_SCALER_OBSERVE_ONLY=.*/E2B_SCALER_OBSERVE_ONLY=true/' \
  "$env_file"
chmod 0600 "$env_file"
echo '{"event":"stateless_scaler_mig_switched","observe_only":true}'
