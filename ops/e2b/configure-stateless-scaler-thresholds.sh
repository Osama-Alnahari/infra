#!/usr/bin/env bash
set -euo pipefail

readonly env_file=/etc/etlaq/e2b-stateless-scaler.env
readonly mode="${1:-observe}"

if [[ "$mode" != "observe" && "$mode" != "enable" ]]; then
  echo "usage: $0 [observe|enable]" >&2
  exit 2
fi

sed -i \
  -e '/^E2B_SCALER_MIN_WORKERS=/d' \
  -e '/^E2B_SCALER_MAX_WORKERS=/d' \
  -e '/^E2B_SCALER_SLOTS_PER_WORKER=/d' \
  -e '/^E2B_SCALER_WORKER_SCALE_OUT_SLOTS=/d' \
  -e '/^E2B_SCALER_WORKER_SATURATION_RESET_SLOTS=/d' \
  -e '/^E2B_SCALER_MIN_FREE_SLOTS=/d' \
  -e '/^E2B_SCALER_LEGACY_CAPACITY=/d' \
  -e '/^E2B_SCALER_OBSERVE_ONLY=/d' \
  "$env_file"

{
  echo 'E2B_SCALER_MIN_WORKERS=0'
  echo 'E2B_SCALER_MAX_WORKERS=5'
  echo 'E2B_SCALER_SLOTS_PER_WORKER=17'
  echo 'E2B_SCALER_WORKER_SCALE_OUT_SLOTS=14'
  echo 'E2B_SCALER_WORKER_SATURATION_RESET_SLOTS=10'
  echo 'E2B_SCALER_MIN_FREE_SLOTS=8'
  echo 'E2B_SCALER_LEGACY_CAPACITY=15'
  if [[ "$mode" == "enable" ]]; then
    echo 'E2B_SCALER_OBSERVE_ONLY=false'
  else
    echo 'E2B_SCALER_OBSERVE_ONLY=true'
  fi
} >>"$env_file"

chmod 0600 "$env_file"
echo "{\"event\":\"stateless_scaler_thresholds_configured\",\"mode\":\"${mode}\"}"
