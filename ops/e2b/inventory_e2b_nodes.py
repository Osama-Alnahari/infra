#!/usr/bin/env python3
import json
import urllib.request
from pathlib import Path


def env_value(name: str) -> str:
    prefix = f"{name}="
    for line in Path("/etc/etlaq/e2b-stateless-scaler.env").read_text().splitlines():
        if line.startswith(prefix):
            return line[len(prefix):]
    raise RuntimeError(f"missing {name}")


request = urllib.request.Request(
    f"{env_value('E2B_SCALER_API_URL')}/nodes",
    headers={"X-Admin-Token": env_value("E2B_SCALER_API_ADMIN_TOKEN")},
)
with urllib.request.urlopen(request, timeout=20) as response:
    nodes = json.load(response)

safe = [{
    "id": str(node.get("id", "")),
    "status": node.get("status"),
    "running": int(node.get("sandboxCount", 0)),
    "starting": int(node.get("sandboxStartingCount", 0)),
} for node in nodes]
print(json.dumps(sorted(safe, key=lambda item: item["id"]), indent=2))
