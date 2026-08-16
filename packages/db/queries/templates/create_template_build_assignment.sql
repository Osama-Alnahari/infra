-- name: CreateTemplateBuildAssignment :execrows
-- FOR SHARE serializes against a concurrent DeleteTemplate so a build can't be
-- attached to a soft-deleted env. 0 rows affected means the template is gone.
WITH snapshot_gc_fence AS MATERIALIZED (
    SELECT pg_advisory_xact_lock_shared(hashtextextended('e2b:snapshot:global-roots', 0))
), active AS (
    SELECT env.id
    FROM "public"."envs" env
    CROSS JOIN snapshot_gc_fence
    WHERE env.id = @template_id AND env.deleted_at IS NULL
    FOR SHARE
)
INSERT INTO "public"."env_build_assignments" (env_id, build_id, tag)
SELECT @template_id, @build_id, @tag::text
WHERE EXISTS (SELECT 1 FROM active);
