package snapshots

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/db/pkg/testutils"
	"github.com/e2b-dev/infra/packages/db/queries"
)

func TestSnapshotGCDeadJobRevivesOnlyForChangedSuccessor(t *testing.T) {
	db := testutils.SetupDatabase(t)
	ctx := t.Context()
	teamID := testutils.CreateTestTeam(t, db)
	baseTemplateID := testutils.CreateTestTemplate(t, db, teamID)
	envID := "snapshot-template-" + uuid.NewString()
	sandboxID := "sandbox-" + uuid.NewString()

	first := testutils.UpsertTestSnapshot(t, ctx, db, envID, sandboxID, teamID, baseTemplateID)
	time.Sleep(10 * time.Millisecond)
	second := testutils.UpsertTestSnapshot(t, ctx, db, envID, sandboxID, teamID, baseTemplateID)
	grace := pgtype.Interval{Valid: true}

	_, err := db.SqlcClient.EnqueueSupersededSnapshotGCJobs(ctx, queries.EnqueueSupersededSnapshotGCJobsParams{
		SnapshotEnvID: envID, SuccessorBuildID: &second.BuildID, GracePeriod: grace,
	})
	require.NoError(t, err)

	err = db.SqlcClient.TestsRawSQL(ctx,
		"UPDATE public.snapshot_gc_jobs SET state = 'dead', attempts = 12, completed_at = now() WHERE snapshot_env_id = $1 AND candidate_build_id = $2",
		envID, first.BuildID,
	)
	require.NoError(t, err)

	// Re-enqueueing the same topology must not create an infinite retry loop.
	_, err = db.SqlcClient.EnqueueSupersededSnapshotGCJobs(ctx, queries.EnqueueSupersededSnapshotGCJobsParams{
		SnapshotEnvID: envID, SuccessorBuildID: &second.BuildID, GracePeriod: grace,
	})
	require.NoError(t, err)
	job := readSnapshotGCJob(t, db, envID, first.BuildID)
	require.Equal(t, "dead", job.state)
	require.EqualValues(t, 12, job.attempts)

	time.Sleep(10 * time.Millisecond)
	third := testutils.UpsertTestSnapshot(t, ctx, db, envID, sandboxID, teamID, baseTemplateID)
	_, err = db.SqlcClient.EnqueueSupersededSnapshotGCJobs(ctx, queries.EnqueueSupersededSnapshotGCJobsParams{
		SnapshotEnvID: envID, SuccessorBuildID: &third.BuildID, GracePeriod: grace,
	})
	require.NoError(t, err)
	job = readSnapshotGCJob(t, db, envID, first.BuildID)
	require.Equal(t, "pending", job.state)
	require.Zero(t, job.attempts)
	require.Equal(t, third.BuildID, job.successor)
	require.False(t, job.completed)

	// A completed audit record is immutable even after a later snapshot appears.
	err = db.SqlcClient.TestsRawSQL(ctx,
		"UPDATE public.snapshot_gc_jobs SET state = 'done', completed_at = now() WHERE snapshot_env_id = $1 AND candidate_build_id = $2",
		envID, first.BuildID,
	)
	require.NoError(t, err)
	time.Sleep(10 * time.Millisecond)
	fourth := testutils.UpsertTestSnapshot(t, ctx, db, envID, sandboxID, teamID, baseTemplateID)
	_, err = db.SqlcClient.EnqueueSupersededSnapshotGCJobs(ctx, queries.EnqueueSupersededSnapshotGCJobsParams{
		SnapshotEnvID: envID, SuccessorBuildID: &fourth.BuildID, GracePeriod: grace,
	})
	require.NoError(t, err)
	job = readSnapshotGCJob(t, db, envID, first.BuildID)
	require.Equal(t, "done", job.state)
	require.Equal(t, third.BuildID, job.successor)
	require.True(t, job.completed)
}

type snapshotGCJobState struct {
	state     string
	attempts  int32
	successor uuid.UUID
	completed bool
}

func readSnapshotGCJob(t *testing.T, db *testutils.Database, envID string, candidate uuid.UUID) snapshotGCJobState {
	t.Helper()
	var result snapshotGCJobState
	err := db.SqlcClient.TestsRawSQLQuery(t.Context(),
		"SELECT state, attempts, successor_build_id, completed_at IS NOT NULL FROM public.snapshot_gc_jobs WHERE snapshot_env_id = $1 AND candidate_build_id = $2",
		func(rows pgx.Rows) error {
			require.True(t, rows.Next())
			return rows.Scan(&result.state, &result.attempts, &result.successor, &result.completed)
		}, envID, candidate,
	)
	require.NoError(t, err)
	return result
}
