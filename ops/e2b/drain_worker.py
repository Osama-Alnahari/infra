#!/usr/bin/env python3
"""Change one E2B worker's placement status without exposing secrets."""

from __future__ import annotations

import os
import sys
from pathlib import Path


def load_environment(path: Path) -> None:
    for raw_line in path.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        os.environ[key] = value


def main() -> int:
    if len(sys.argv) not in (2, 3):
        raise SystemExit("usage: drain_worker.py WORKER_ID [draining|ready]")
    load_environment(Path("/etc/etlaq/e2b-stateless-scaler.env"))
    sys.path.insert(0, "/usr/local/lib/etlaq-e2b-scaler")
    import stateless_capacity_controller as scaler

    worker_id = sys.argv[1]
    status = sys.argv[2] if len(sys.argv) == 3 else "draining"
    if status not in {"draining", "ready"}:
        raise SystemExit("status must be draining or ready")
    config = scaler.load_config()
    scaler.set_api_node_status(config, worker_id, status)
    eligibility = "ineligible" if status == "draining" else "eligible"
    scaler.set_nomad_eligibility(config, worker_id, eligibility)
    print(f"worker={worker_id} status={status} eligibility={eligibility}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
