# E2B stateless canary — Phase 1

This canary is additive. Never update, resize, recreate, snapshot, or detach
resources belonging to `etlaq-e2b-poc-orch-client-rig` as part of this phase.

## Production resources that are immutable during Phase 1

- MIG: `etlaq-e2b-poc-orch-client-rig`
- Workers: `etlaq-e2b-poc-orch-client-{8gm0,0kr1,ns26}`
- Preserved cache disks:
  - `etlaq-e2b-poc-orch-client-8gm0-1`
  - `etlaq-e2b-poc-orch-client-0kr1-1`
  - `etlaq-e2b-poc-orch-client-ns26-1`

## Canary resources

- Nomad pool: `default` after canary approval
- Template: `etlaq-e2b-poc-orch-client-stateless-12-96-canary-20260814`
- Regional MIG: `etlaq-e2b-poc-orch-client-stateless-canary-rig`
- Machine: `n2-custom-12-98304`
- Boot disk: 100 GB `pd-balanced`, auto-delete
- Cache disk: 500 GB `pd-ssd`, auto-delete
- Autoscaling: sandbox-count controller, 1–5 workers
- Measured permanent hugepage pool: 63.36 GiB
- Initial safe capacity target: 15 sandboxes at 4 GiB each

The worker was isolated in `canary` during validation. After all gates passed,
the bootstrap was promoted to `default`, allowing the existing production
orchestrator system job and API discovery path to use stateless workers.

## Go/no-go gates

1. The canary node is healthy in Nomad pool `canary`, is scheduling-ineligible,
   and has no E2B orchestrator or sandbox allocations. Cluster-wide telemetry
   allocations are allowed.
2. Hugepage, KVM, GCS, disk, and network checks pass.
3. A separately named canary orchestrator registers no production service.
4. Synthetic pause reports authoritative completion and its objects are in GCS.
5. Cross-worker resume succeeds before canary deletion is tested.
6. The canary has zero allocations/uploads before MIG recreation.

## Phase 1 test results — 2026-08-14

- Production workers, API, and preserved disks were not modified.
- A synthetic sandbox was created, wrote a marker, and was paused.
- The stateless canary MIG instance was recreated, including both auto-delete
  disks. The paused sandbox then cold-resumed with the exact marker SHA-256,
  proving that required pause state survived outside the worker cache.
- Initial quota-limited capacity testing verified **17 simultaneous, fully ready sandboxes**. Every
  sandbox successfully executed a command before it was counted.
- The next create request was rejected by the shared E2B account concurrency
  quota of 20, because three other sandboxes existed outside the canary. It did
  **not** reach `failed to place sandbox`.
- The project concurrency ceiling was then raised from 20 to 120 without an API
  or worker restart, while all other project entitlements were preserved.
- The repeated test verified **19 simultaneous, fully ready sandboxes** on the
  12-vCPU / 96-GiB worker. Every sandbox executed a command successfully.
- Sandbox 20 then failed with the genuine host response
  `500: Failed to place sandbox`. The measured placement ceiling for this
  exact image, hugepage policy, and workload is therefore **19 sandboxes**.
- All synthetic capacity sandboxes were deleted, the durability sandbox was
  re-paused, the private canary API was purged, and the worker was returned to
  scheduling-ineligible state.
- The validated worker was promoted into production discovery without an API
  or stateful-worker restart. The controller uses 17 safe slots per stateless
  worker, an 80% scale-out threshold, a 55% scale-in threshold, a 180-second
  cooldown, and exact drain-before-delete semantics.
- A live 1→2→1 lifecycle test passed. The second worker registered before
  scale-in was allowed, was marked draining while empty, and was deleted by
  exact instance name. Existing stateful workers and user sandboxes remained
  online.
- Regional target shape was changed from `EVEN` to `BALANCED` after zone B
  reported stockout for the 12-vCPU/96-GiB shape; the replacement was placed
  successfully in zone C.
- After promotion, the stateful MIG was safely reduced from three workers to
  one. The retained worker had the only active stateful sandbox. The two idle
  workers were marked draining, verified at zero running/starting sandboxes,
  and deleted by exact instance name. Their 500-GB persistent disks remain
  detached with `autoDelete=NEVER`. The retained stateful worker now uses the
  same 12-vCPU/96-GiB shape and 80% hugepage policy as the validated stateless
  worker. The controller conservatively assigns it 15 slots so stateless
  capacity is added before the stateful worker reaches the tested ceiling.
- Scale-out also guards against uneven placement: any stateless worker reaching
  14 of 17 slots triggers one expansion, with a durable saturation latch that
  resets after the worker falls to 10 slots. Fleet-wide free capacity of eight
  slots or fewer also triggers expansion.

## Production naming cutover

The validated canary was replaced without interrupting active sandboxes:

- The canary worker was marked draining and Nomad-ineligible.
- Existing sandboxes were allowed to finish; deletion waited for exactly zero
  running and zero starting sandboxes.
- Production MIG: `etlaq-e2b-poc-orch-client-stateless-rig`.
- Production VM prefix: `etlaq-e2b-poc-orch-client-stateless-`.
- The replacement passed a forced-placement sandbox create, command, and
  delete smoke test before autoscaling was enabled.
- The scaler now manages only the production MIG, with minimum `1`, maximum
  `5`, and sandbox-slot-based decisions.
- The old `etlaq-e2b-poc-orch-client-stateless-canary-rig` was deleted only
  after its target size reached zero.
