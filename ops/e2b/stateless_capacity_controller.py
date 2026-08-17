#!/usr/bin/env python3
"""Capacity-driven controller for Etlaq's stateless E2B worker MIG.

The controller owns only the stateless worker group. Scale-out is based on
authoritative sandbox counts returned by the E2B API. Scale-in is two-phase:
mark one exact worker draining, then delete that exact MIG instance only after
the API reports zero running and zero starting sandboxes.
"""

from __future__ import annotations

import dataclasses
import fcntl
import json
import os
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any


@dataclasses.dataclass(frozen=True)
class Config:
    project: str
    region: str
    mig: str
    worker_prefix: str
    api_url: str
    api_admin_token: str
    nomad_url: str
    nomad_token: str
    min_workers: int = 1
    max_workers: int = 5
    slots_per_worker: int = 17
    legacy_capacity: int = 15
    scale_out_utilization: float = 0.80
    worker_scale_out_slots: int = 14
    worker_saturation_reset_slots: int = 10
    min_free_slots: int = 8
    scale_in_utilization: float = 0.55
    cooldown_seconds: int = 180
    observe_only: bool = True
    state_path: Path = Path("/var/lib/etlaq-e2b-scaler/state.json")
    lock_path: Path = Path("/run/lock/etlaq-e2b-scaler.lock")


def load_config() -> Config:
    required = {
        name: os.environ[name]
        for name in (
            "E2B_SCALER_PROJECT",
            "E2B_SCALER_REGION",
            "E2B_SCALER_MIG",
            "E2B_SCALER_WORKER_PREFIX",
            "E2B_SCALER_API_ADMIN_TOKEN",
            "E2B_SCALER_NOMAD_TOKEN",
        )
    }
    return Config(
        project=required["E2B_SCALER_PROJECT"],
        region=required["E2B_SCALER_REGION"],
        mig=required["E2B_SCALER_MIG"],
        worker_prefix=required["E2B_SCALER_WORKER_PREFIX"],
        api_url=os.getenv("E2B_SCALER_API_URL", "https://api.sandbox.etlaq.sa"),
        api_admin_token=required["E2B_SCALER_API_ADMIN_TOKEN"],
        nomad_url=os.getenv("E2B_SCALER_NOMAD_URL", "http://127.0.0.1:4646"),
        nomad_token=required["E2B_SCALER_NOMAD_TOKEN"],
        min_workers=int(os.getenv("E2B_SCALER_MIN_WORKERS", "1")),
        max_workers=int(os.getenv("E2B_SCALER_MAX_WORKERS", "5")),
        slots_per_worker=int(os.getenv("E2B_SCALER_SLOTS_PER_WORKER", "17")),
        legacy_capacity=int(os.getenv("E2B_SCALER_LEGACY_CAPACITY", "15")),
        scale_out_utilization=float(os.getenv("E2B_SCALER_SCALE_OUT_UTILIZATION", "0.80")),
        worker_scale_out_slots=int(os.getenv("E2B_SCALER_WORKER_SCALE_OUT_SLOTS", "14")),
        worker_saturation_reset_slots=int(os.getenv("E2B_SCALER_WORKER_SATURATION_RESET_SLOTS", "10")),
        min_free_slots=int(os.getenv("E2B_SCALER_MIN_FREE_SLOTS", "8")),
        scale_in_utilization=float(os.getenv("E2B_SCALER_SCALE_IN_UTILIZATION", "0.55")),
        cooldown_seconds=int(os.getenv("E2B_SCALER_COOLDOWN_SECONDS", "180")),
        observe_only=os.getenv("E2B_SCALER_OBSERVE_ONLY", "true").lower() != "false",
    )


def http_json(url: str, headers: dict[str, str], method: str = "GET", body: Any = None) -> Any:
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method=method, headers={**headers, "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=20) as response:
        raw = response.read()
        return json.loads(raw) if raw else None


def run_gcloud(config: Config, *args: str) -> str:
    gcloud = os.getenv("E2B_SCALER_GCLOUD", "gcloud")
    command = [gcloud, "compute", "instance-groups", "managed", *args, "--project", config.project, "--quiet"]
    try:
        return subprocess.run(command, check=True, capture_output=True, text=True, timeout=180).stdout
    except subprocess.CalledProcessError as error:
        detail = (error.stderr or "gcloud command failed").strip().replace("\n", " ")[-500:]
        raise RuntimeError(detail) from error


def mig_size(config: Config) -> int:
    value = run_gcloud(config, "describe", config.mig, "--region", config.region, "--format=value(targetSize)")
    return int(value.strip())


def mig_instances(config: Config) -> set[str]:
    output = run_gcloud(
        config,
        "list-instances",
        config.mig,
        "--region",
        config.region,
        "--format=value(instance.basename())",
    )
    return {line.strip() for line in output.splitlines() if line.strip()}


def resize(config: Config, size: int) -> None:
    run_gcloud(config, "resize", config.mig, "--region", config.region, "--size", str(size))


def delete_instance(config: Config, instance: str) -> None:
    run_gcloud(config, "delete-instances", config.mig, "--region", config.region, "--instances", instance)


def load_state(config: Config) -> dict[str, Any]:
    try:
        return json.loads(config.state_path.read_text())
    except (FileNotFoundError, json.JSONDecodeError):
        return {"last_mutation": 0, "draining": None, "saturation_scaled": []}


def save_state(config: Config, state: dict[str, Any]) -> None:
    config.state_path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(dir=config.state_path.parent, prefix="state.", text=True)
    try:
        with os.fdopen(fd, "w") as handle:
            json.dump(state, handle, sort_keys=True)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, config.state_path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def capacity_without_one(config: Config, workers: int) -> int:
    return config.legacy_capacity + max(0, workers - 1) * config.slots_per_worker


def decide(
    config: Config,
    workers: int,
    nodes: list[dict[str, Any]],
    state: dict[str, Any],
    now: int,
    managed_workers: set[str] | None = None,
) -> dict[str, Any]:
    running = sum(int(node.get("sandboxCount", 0)) for node in nodes)
    starting = sum(int(node.get("sandboxStartingCount", 0)) for node in nodes)
    # The production API returns all nodes. Include all active work in the fleet
    # pressure calculation, while only stateless nodes are candidates to drain.
    active = running + starting
    fleet_capacity = config.legacy_capacity + workers * config.slots_per_worker
    stateless = [node for node in nodes if str(node.get("id", "")).startswith(config.worker_prefix)]
    if managed_workers is not None:
        stateless = [node for node in stateless if str(node.get("id", "")) in managed_workers]
    ready_stateless = [node for node in stateless if str(node.get("status", "")).lower() in {"ready", "healthy"}]
    stateless_load = {
        str(node.get("id", "")): int(node.get("sandboxCount", 0)) + int(node.get("sandboxStartingCount", 0))
        for node in ready_stateless
    }
    peak_stateless = max(stateless_load.values(), default=0)
    free_slots = max(0, fleet_capacity - active)
    draining_id = state.get("draining")
    draining_node = next((node for node in stateless if node.get("id") == draining_id), None)

    if draining_id and managed_workers is not None and draining_id not in managed_workers:
        return {"action": "clear_draining", "reason": "worker_absent_from_mig", "active": active}

    if draining_id:
        if draining_node is None:
            return {"action": "clear_draining", "reason": "worker_absent", "active": active}
        empty = int(draining_node.get("sandboxCount", 0)) == 0 and int(draining_node.get("sandboxStartingCount", 0)) == 0
        if empty and workers > config.min_workers:
            return {"action": "delete", "worker": draining_id, "reason": "drained_empty", "active": active}
        return {"action": "wait_draining", "worker": draining_id, "reason": "worker_not_empty", "active": active}

    if now - int(state.get("last_mutation", 0)) < config.cooldown_seconds:
        return {"action": "none", "reason": "cooldown", "active": active}

    if len(ready_stateless) < workers:
        return {"action": "none", "reason": "worker_registration_pending", "active": active}

    acknowledged = set(state.get("saturation_scaled", []))
    newly_saturated = sorted(
        worker for worker, load in stateless_load.items()
        if load >= config.worker_scale_out_slots and worker not in acknowledged
    )
    if workers < config.max_workers and newly_saturated:
        return {
            "action": "scale_out",
            "size": workers + 1,
            "reason": "worker_saturation",
            "worker": newly_saturated[0],
            "active": active,
            "free_slots": free_slots,
            "peak_stateless": peak_stateless,
        }

    if workers < config.max_workers and free_slots <= config.min_free_slots:
        return {
            "action": "scale_out",
            "size": workers + 1,
            "reason": "fleet_headroom",
            "active": active,
            "free_slots": free_slots,
            "peak_stateless": peak_stateless,
        }

    if workers < config.max_workers and active >= fleet_capacity * config.scale_out_utilization:
        return {"action": "scale_out", "size": workers + 1, "reason": "slot_pressure", "active": active}

    remaining_capacity = capacity_without_one(config, workers)
    if workers > config.min_workers and active <= remaining_capacity * config.scale_in_utilization and stateless:
        candidate = min(stateless, key=lambda node: (int(node.get("sandboxCount", 0)) + int(node.get("sandboxStartingCount", 0)), str(node.get("id"))))
        return {"action": "start_draining", "worker": candidate["id"], "reason": "low_slot_pressure", "active": active}

    return {"action": "none", "reason": "within_band", "active": active}


def set_api_node_status(config: Config, node_id: str, status: str) -> None:
    http_json(
        f"{config.api_url}/nodes/{node_id}",
        {"X-Admin-Token": config.api_admin_token},
        method="POST",
        body={"status": status},
    )


def set_nomad_eligibility(config: Config, node_name: str, eligibility: str) -> None:
    nodes = http_json(f"{config.nomad_url}/v1/nodes", {"X-Nomad-Token": config.nomad_token})
    match = next((node for node in nodes if node.get("Name") == node_name), None)
    if not match:
        raise RuntimeError(f"Nomad node not found for {node_name}")
    http_json(
        f"{config.nomad_url}/v1/node/{match['ID']}/eligibility",
        {"X-Nomad-Token": config.nomad_token},
        method="POST",
        body={"Eligibility": eligibility},
    )


def cycle(config: Config) -> dict[str, Any]:
    workers = mig_size(config)
    managed_workers = mig_instances(config)
    nodes = http_json(f"{config.api_url}/nodes", {"X-Admin-Token": config.api_admin_token})
    state = load_state(config)
    live_load = {
        str(node.get("id", "")): int(node.get("sandboxCount", 0)) + int(node.get("sandboxStartingCount", 0))
        for node in nodes
        if str(node.get("id", "")).startswith(config.worker_prefix)
    }
    state["saturation_scaled"] = [
        worker for worker in state.get("saturation_scaled", [])
        if live_load.get(worker, 0) > config.worker_saturation_reset_slots
    ]
    now = int(time.time())
    decision = decide(config, workers, nodes, state, now, managed_workers)
    event = {"event": "e2b_scaler_decision", "workers": workers, "observe_only": config.observe_only, **decision}
    print(json.dumps(event, sort_keys=True), flush=True)
    if config.observe_only:
        return event

    action = decision["action"]
    if action == "scale_out":
        resize(config, int(decision["size"]))
        if decision.get("reason") == "worker_saturation":
            state.setdefault("saturation_scaled", []).append(str(decision["worker"]))
            state["saturation_scaled"] = sorted(set(state["saturation_scaled"]))
        state["last_mutation"] = now
    elif action == "start_draining":
        worker = str(decision["worker"])
        set_api_node_status(config, worker, "draining")
        set_nomad_eligibility(config, worker, "ineligible")
        state["draining"] = worker
        state["last_mutation"] = now
    elif action == "delete":
        worker = str(decision["worker"])
        # GCP's managed delete operation removes this exact instance and
        # automatically decreases targetSize by one.
        delete_instance(config, worker)
        state["draining"] = None
        state["last_mutation"] = now
    elif action == "clear_draining":
        state["draining"] = None
    save_state(config, state)
    return event


def main() -> int:
    config = load_config()
    if not (1 <= config.min_workers <= config.max_workers <= 5):
        raise ValueError("worker bounds must satisfy 1 <= min <= max <= 5")
    if config.slots_per_worker != 17:
        raise ValueError("slots_per_worker must remain at the verified safe value 17")
    if not (0 < config.worker_saturation_reset_slots < config.worker_scale_out_slots <= config.slots_per_worker):
        raise ValueError("worker saturation thresholds must satisfy 0 < reset < scale-out <= slots")
    if not (1 <= config.min_free_slots < config.slots_per_worker):
        raise ValueError("min_free_slots must be between 1 and slots_per_worker - 1")
    config.lock_path.parent.mkdir(parents=True, exist_ok=True)
    with config.lock_path.open("w") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            print(json.dumps({"event": "e2b_scaler_skipped", "reason": "cycle_already_running"}))
            return 0
        cycle(config)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (subprocess.SubprocessError, urllib.error.URLError, RuntimeError, ValueError) as error:
        detail = str(error).replace("\n", " ")[-500:]
        print(json.dumps({"event": "e2b_scaler_failed", "error_type": type(error).__name__, "detail": detail}), file=sys.stderr)
        raise SystemExit(1) from error
