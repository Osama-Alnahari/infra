-- +goose Up
CREATE TABLE public.snapshot_gc_jobs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Deliberately not foreign-keyed: completed/dead jobs are the durable
    -- deletion audit even after an environment or build record is removed.
    snapshot_env_id text NOT NULL,
    sandbox_id text NOT NULL,
    -- NULL for a soft-deleted environment: there is no local successor, so the
    -- executor relies on the complete globally verified active-head set.
    successor_build_id uuid NULL,
    candidate_build_id uuid NOT NULL,
    state text NOT NULL DEFAULT 'pending',
    not_before timestamptz NOT NULL,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    attempts integer NOT NULL DEFAULT 0,
    lease_owner text NULL,
    lease_expires_at timestamptz NULL,
    last_error text NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz NULL,
    CONSTRAINT snapshot_gc_jobs_distinct_builds CHECK (successor_build_id IS NULL OR successor_build_id <> candidate_build_id),
    CONSTRAINT snapshot_gc_jobs_valid_state CHECK (state IN ('pending', 'leased', 'retry', 'done', 'dead')),
    CONSTRAINT snapshot_gc_jobs_attempts_nonnegative CHECK (attempts >= 0),
    CONSTRAINT snapshot_gc_jobs_candidate_once UNIQUE (snapshot_env_id, candidate_build_id)
);

CREATE INDEX snapshot_gc_jobs_claim_idx
    ON public.snapshot_gc_jobs (next_attempt_at, not_before, created_at)
    WHERE state IN ('pending', 'retry', 'leased');

CREATE INDEX snapshot_gc_jobs_successor_idx
    ON public.snapshot_gc_jobs (successor_build_id)
    WHERE state IN ('pending', 'retry', 'leased');

-- +goose Down
DROP TABLE IF EXISTS public.snapshot_gc_jobs;
