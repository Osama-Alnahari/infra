-- name: EnqueueSupersededSnapshotGCJobs :execrows
WITH current_assignment AS (
    SELECT eba.env_id, eba.created_at
    FROM public.env_build_assignments eba
    JOIN public.env_builds eb ON eb.id = eba.build_id
    WHERE eba.env_id = @snapshot_env_id
      AND eba.build_id = @successor_build_id
      AND eba.tag = 'default'
      AND eb.status_group = 'ready'
), current_tip AS (
    SELECT eba.build_id
    FROM public.env_build_assignments eba
    JOIN public.env_builds eb ON eb.id = eba.build_id
    WHERE eba.env_id = @snapshot_env_id
      AND eba.tag = 'default'
      AND eb.status_group = 'ready'
    ORDER BY eba.created_at DESC, eba.build_id DESC
    LIMIT 1
)
INSERT INTO public.snapshot_gc_jobs (
    snapshot_env_id, sandbox_id, successor_build_id, candidate_build_id,
    not_before, next_attempt_at
)
SELECT s.env_id, s.sandbox_id, @successor_build_id, old.build_id,
       now() + @grace_period::interval, now() + @grace_period::interval
FROM public.snapshots s
JOIN current_assignment current ON current.env_id = s.env_id
JOIN current_tip tip ON tip.build_id = @successor_build_id
JOIN public.env_build_assignments old
  ON old.env_id = s.env_id
 AND old.tag = 'default'
 AND (old.created_at, old.build_id) < (current.created_at, @successor_build_id)
JOIN public.env_builds old_build
  ON old_build.id = old.build_id
 AND old_build.status_group = 'ready'
WHERE s.env_id = @snapshot_env_id
ON CONFLICT (snapshot_env_id, candidate_build_id) DO UPDATE
SET successor_build_id = EXCLUDED.successor_build_id,
    state = 'pending',
    not_before = EXCLUDED.not_before,
    next_attempt_at = EXCLUDED.next_attempt_at,
    attempts = 0,
    lease_owner = NULL,
    lease_expires_at = NULL,
    last_error = NULL,
    completed_at = NULL,
    updated_at = now()
WHERE snapshot_gc_jobs.state = 'dead'
  AND snapshot_gc_jobs.successor_build_id IS DISTINCT FROM EXCLUDED.successor_build_id;

-- name: ReconcileSnapshotGCJobs :execrows
WITH missing AS (
    SELECT s.env_id AS snapshot_env_id,
           s.sandbox_id,
           CASE WHEN env.deleted_at IS NULL THEN tip.build_id ELSE NULL END::uuid AS successor_build_id,
           old.build_id AS candidate_build_id
    FROM public.snapshots s
    -- Include soft-deleted snapshot environments: their old GCS prefixes are
    -- still physical cleanup candidates even though they are no longer roots.
    JOIN public.envs env ON env.id = s.env_id
    JOIN LATERAL (
        SELECT eba.build_id, eba.created_at
        FROM public.env_build_assignments eba
        JOIN public.env_builds eb ON eb.id = eba.build_id AND eb.status_group = 'ready'
        WHERE eba.env_id = s.env_id AND eba.tag = 'default'
        ORDER BY eba.created_at DESC, eba.build_id DESC
        LIMIT 1
    ) tip ON TRUE
    JOIN public.env_build_assignments old
      ON old.env_id = s.env_id
     AND old.tag = 'default'
     AND (
       env.deleted_at IS NOT NULL
       OR (old.created_at, old.build_id) < (tip.created_at, tip.build_id)
     )
    JOIN public.env_builds old_build
      ON old_build.id = old.build_id
     AND old_build.status_group = 'ready'
    LEFT JOIN public.snapshot_gc_jobs job
      ON job.snapshot_env_id = s.env_id
     AND job.candidate_build_id = old.build_id
    WHERE job.id IS NULL
       OR (
         job.state = 'dead'
         AND job.successor_build_id IS DISTINCT FROM
           CASE WHEN env.deleted_at IS NULL THEN tip.build_id ELSE NULL END::uuid
       )
    ORDER BY old.created_at ASC, old.build_id ASC
    LIMIT @batch_size
)
INSERT INTO public.snapshot_gc_jobs (
    snapshot_env_id, sandbox_id, successor_build_id, candidate_build_id,
    not_before, next_attempt_at
)
SELECT snapshot_env_id, sandbox_id, successor_build_id, candidate_build_id,
       now() + @grace_period::interval, now() + @grace_period::interval
FROM missing
ON CONFLICT (snapshot_env_id, candidate_build_id) DO UPDATE
SET successor_build_id = EXCLUDED.successor_build_id,
    state = 'pending',
    not_before = EXCLUDED.not_before,
    next_attempt_at = EXCLUDED.next_attempt_at,
    attempts = 0,
    lease_owner = NULL,
    lease_expires_at = NULL,
    last_error = NULL,
    completed_at = NULL,
    updated_at = now()
WHERE snapshot_gc_jobs.state = 'dead'
  AND snapshot_gc_jobs.successor_build_id IS DISTINCT FROM EXCLUDED.successor_build_id;

-- name: ClaimSnapshotGCJobs :many
WITH claimable AS (
    SELECT id
    FROM public.snapshot_gc_jobs
    WHERE not_before <= now()
      AND next_attempt_at <= now()
      AND (
        state IN ('pending', 'retry')
        OR (state = 'leased' AND lease_expires_at < now())
      )
    ORDER BY next_attempt_at, created_at
    LIMIT @batch_size
    FOR UPDATE SKIP LOCKED
)
UPDATE public.snapshot_gc_jobs job
SET state = 'leased',
    lease_owner = @lease_owner,
    lease_expires_at = now() + @lease_duration::interval,
    updated_at = now()
FROM claimable
WHERE job.id = claimable.id
RETURNING job.*;

-- name: CompleteSnapshotGCJob :execrows
UPDATE public.snapshot_gc_jobs
SET state = 'done', lease_owner = NULL, lease_expires_at = NULL,
    last_error = NULL, completed_at = now(), updated_at = now()
WHERE id = @id AND state = 'leased' AND lease_owner = @lease_owner;

-- name: OwnsSnapshotGCLease :one
-- Fence the irreversible storage mutation. A worker whose lease expired while
-- waiting for the advisory lock must not delete after another worker reclaimed
-- the same job.
SELECT EXISTS (
    SELECT 1
    FROM public.snapshot_gc_jobs
    WHERE id = @id
      AND state = 'leased'
      AND lease_owner = @lease_owner
      AND lease_expires_at > now()
)::boolean;

-- name: RenewSnapshotGCLease :execrows
-- The UPDATE both verifies ownership and keeps this row locked until the GC
-- transaction commits. ClaimSnapshotGCJobs uses FOR UPDATE SKIP LOCKED, so no
-- second worker can reclaim the job while the storage RPC is in flight.
UPDATE public.snapshot_gc_jobs
SET lease_expires_at = now() + @lease_duration::interval,
    updated_at = now()
WHERE id = @id
  AND state = 'leased'
  AND lease_owner = @lease_owner
  AND lease_expires_at > now();

-- name: RetrySnapshotGCJob :execrows
UPDATE public.snapshot_gc_jobs
SET attempts = attempts + 1,
    state = CASE WHEN attempts + 1 >= @max_attempts THEN 'dead' ELSE 'retry' END,
    next_attempt_at = @next_attempt_at,
    lease_owner = NULL,
    lease_expires_at = NULL,
    last_error = left(@last_error, 4000),
    completed_at = CASE WHEN attempts + 1 >= @max_attempts THEN now() ELSE NULL END,
    updated_at = now()
WHERE id = @id AND state = 'leased' AND lease_owner = @lease_owner;

-- name: DeferSnapshotGCJob :execrows
UPDATE public.snapshot_gc_jobs
SET state = 'retry', next_attempt_at = @next_attempt_at,
    lease_owner = NULL, lease_expires_at = NULL,
    last_error = left(@last_error, 4000), updated_at = now()
WHERE id = @id AND state = 'leased' AND lease_owner = @lease_owner;

-- name: CountPendingSnapshotGCJobs :one
SELECT count(*)::bigint
FROM public.snapshot_gc_jobs
WHERE state IN ('pending', 'retry', 'leased');

-- name: ListSnapshotGCProtectedBuilds :many
-- Every currently-addressable non-snapshot assignment is a root. Snapshot
-- environments retain only their newest ready default assignment as a root;
-- older assignments are precisely the generations this queue may collect.
WITH snapshot_tips AS (
    SELECT DISTINCT ON (eba.env_id) eba.build_id
    FROM public.env_build_assignments eba
    JOIN public.active_envs env ON env.id = eba.env_id AND env.source = 'snapshot'
    JOIN public.env_builds eb ON eb.id = eba.build_id AND eb.status_group = 'ready'
    WHERE eba.tag = 'default'
    ORDER BY eba.env_id, eba.created_at DESC, eba.build_id DESC
), assigned_template_builds AS (
    SELECT DISTINCT eba.build_id
    FROM public.env_build_assignments eba
    JOIN public.active_envs env ON env.id = eba.env_id AND env.source <> 'snapshot'
    JOIN public.env_builds eb ON eb.id = eba.build_id AND eb.status_group = 'ready'
)
SELECT build_id FROM snapshot_tips
UNION
SELECT build_id FROM assigned_template_builds;

-- name: ListCurrentSnapshotGCHeads :many
-- Verify every active default-assignment head that shares this storage
-- namespace. Template and snapshot-template heads can carry dependencies from
-- a pause snapshot, so excluding them would make the reachability proof unsafe.
SELECT DISTINCT ON (eba.env_id)
       eba.env_id,
       eba.build_id,
       COALESCE((s.config->>'filesystemOnly')::boolean, false)::boolean AS filesystem_only
FROM public.env_build_assignments eba
JOIN public.active_envs env ON env.id = eba.env_id
JOIN public.env_builds eb ON eb.id = eba.build_id AND eb.status_group = 'ready'
LEFT JOIN public.snapshots s ON s.env_id = eba.env_id
WHERE eba.tag = 'default'
ORDER BY eba.env_id, eba.created_at DESC, eba.build_id DESC;

-- name: HasInProgressSnapshotBuilds :one
-- An in-progress checkpoint has no final dependency header yet. Under the
-- global advisory fence GC must defer until its reachability can be proven.
SELECT EXISTS (
    SELECT 1
    FROM public.env_build_assignments eba
    JOIN public.envs env ON env.id = eba.env_id AND env.source = 'snapshot'
    JOIN public.env_builds eb ON eb.id = eba.build_id
    WHERE eba.tag = 'default' AND eb.status_group = 'in_progress'
)::boolean;
