-- name: CreateSnapshotTemplateEnv :one
-- Creates a snapshot_template env entry with source='snapshot_template' and links it to an existing build
-- This is used after UpsertSnapshot to create a persistent snapshot template
WITH snapshot_gc_fence AS MATERIALIZED (
    SELECT pg_advisory_xact_lock_shared(hashtextextended('e2b:snapshot:global-roots', 0))
),

new_env AS (
    INSERT INTO "public"."envs" (id, public, created_by, team_id, updated_at, source, cluster_id)
    SELECT @snapshot_id, FALSE, NULL, @team_id, now(), 'snapshot_template', @cluster_id
    FROM snapshot_gc_fence
    RETURNING id
),

snapshot_template AS (
    INSERT INTO "public"."snapshot_templates" (env_id, sandbox_id, origin_node_id, build_id)
    VALUES (
        (SELECT id FROM new_env),
        @sandbox_id,
        @origin_node_id,
        @build_id
    )
),

build_assignment AS (
    INSERT INTO "public"."env_build_assignments" (env_id, build_id, tag)
    VALUES (
        (SELECT id FROM new_env),
        @build_id,
        @tag
    )
    RETURNING env_id as snapshot_id
)

SELECT snapshot_id FROM build_assignment;
