package snapshotgc

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/db/queries"
)

type fakeStore struct {
	mu                           sync.Mutex
	jobs                         []queries.SnapshotGcJob
	heads                        []queries.ListCurrentSnapshotGCHeadsRow
	protected                    []uuid.UUID
	completed, retried, deferred []uuid.UUID
	inProgress                   bool
	leaseLost                    bool
	renewLost                    bool
	claimBatch                   int32
	enqueuedGrace                time.Duration
}

func (f *fakeStore) EnqueueSupersededSnapshotGCJobs(_ context.Context, p queries.EnqueueSupersededSnapshotGCJobsParams) (int64, error) {
	f.mu.Lock()
	f.enqueuedGrace = time.Duration(p.GracePeriod.Microseconds) * time.Microsecond
	f.mu.Unlock()
	return 1, nil
}
func (f *fakeStore) ReconcileSnapshotGCJobs(context.Context, queries.ReconcileSnapshotGCJobsParams) (int64, error) {
	return 0, nil
}
func (f *fakeStore) ClaimSnapshotGCJobs(_ context.Context, p queries.ClaimSnapshotGCJobsParams) ([]queries.SnapshotGcJob, error) {
	f.mu.Lock()
	f.claimBatch = p.BatchSize
	f.mu.Unlock()
	return f.jobs, nil
}
func (f *fakeStore) CompleteSnapshotGCJob(_ context.Context, p queries.CompleteSnapshotGCJobParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = append(f.completed, p.ID)
	return 1, nil
}
func (f *fakeStore) OwnsSnapshotGCLease(context.Context, queries.OwnsSnapshotGCLeaseParams) (bool, error) {
	return !f.leaseLost, nil
}
func (f *fakeStore) RenewSnapshotGCLease(context.Context, queries.RenewSnapshotGCLeaseParams) (int64, error) {
	if f.leaseLost || f.renewLost {
		return 0, nil
	}
	return 1, nil
}
func (f *fakeStore) RetrySnapshotGCJob(_ context.Context, p queries.RetrySnapshotGCJobParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retried = append(f.retried, p.ID)
	return 1, nil
}
func (f *fakeStore) DeferSnapshotGCJob(_ context.Context, p queries.DeferSnapshotGCJobParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deferred = append(f.deferred, p.ID)
	return 1, nil
}
func (f *fakeStore) ListSnapshotGCProtectedBuilds(context.Context) ([]uuid.UUID, error) {
	return f.protected, nil
}
func (f *fakeStore) ListCurrentSnapshotGCHeads(context.Context) ([]queries.ListCurrentSnapshotGCHeadsRow, error) {
	return f.heads, nil
}
func (f *fakeStore) HasInProgressSnapshotBuilds(context.Context) (bool, error) {
	return f.inProgress, nil
}

type fakeExecutor struct {
	request ExecuteRequest
	result  ExecuteResult
}

func (f *fakeExecutor) ExecuteSnapshotGC(_ context.Context, r ExecuteRequest) (ExecuteResult, error) {
	f.request = r
	return f.result, nil
}

func TestProcessPassesAllGlobalRootsAndCompletesOnlyDeleted(t *testing.T) {
	jobID, candidate, head, templateHead, protected := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	store := &fakeStore{jobs: []queries.SnapshotGcJob{{ID: jobID, SnapshotEnvID: "env-a", CandidateBuildID: candidate, Attempts: 1}}, heads: []queries.ListCurrentSnapshotGCHeadsRow{{EnvID: "env-a", BuildID: head}, {EnvID: "template-a", BuildID: templateHead}}, protected: []uuid.UUID{protected}}
	executor := &fakeExecutor{result: ExecuteResult{Deleted: map[uuid.UUID]bool{candidate: true}}}
	w := New(store, executor, nil, "test")
	lockHeld := false
	w.withSnapshotLock = func(ctx context.Context, _ string, fn func(Store) error) error {
		lockHeld = true
		defer func() { lockHeld = false }()
		return fn(store)
	}
	originalExecutor := executor
	w.SetExecutor(executorFunc(func(ctx context.Context, req ExecuteRequest) (ExecuteResult, error) {
		require.True(t, lockHeld, "storage mutation must run under the snapshot advisory lock")
		return originalExecutor.ExecuteSnapshotGC(ctx, req)
	}))
	w.deleteEnabled = func(context.Context) bool { return true }
	w.process(t.Context())
	require.Equal(t, []Head{{BuildID: head}, {BuildID: templateHead}}, executor.request.Heads)
	require.Equal(t, []uuid.UUID{protected}, executor.request.GlobalProtectedBuildIDs)
	require.Equal(t, []uuid.UUID{jobID}, store.completed)
	require.Empty(t, store.retried)
	require.Empty(t, store.deferred)
}

type executorFunc func(context.Context, ExecuteRequest) (ExecuteResult, error)

func (f executorFunc) ExecuteSnapshotGC(ctx context.Context, req ExecuteRequest) (ExecuteResult, error) {
	return f(ctx, req)
}

func TestProcessKeepsDryRunWorkRetryable(t *testing.T) {
	jobID, candidate, head := uuid.New(), uuid.New(), uuid.New()
	store := &fakeStore{jobs: []queries.SnapshotGcJob{{ID: jobID, SnapshotEnvID: "env-a", CandidateBuildID: candidate, Attempts: 1}}, heads: []queries.ListCurrentSnapshotGCHeadsRow{{EnvID: "env-a", BuildID: head}}}
	executor := &fakeExecutor{result: ExecuteResult{Deleted: map[uuid.UUID]bool{candidate: true}}}
	w := New(store, executor, nil, "test")
	w.withSnapshotLock = func(ctx context.Context, _ string, fn func(Store) error) error { return fn(store) }
	w.now = func() time.Time { return time.Unix(0, 0) }
	w.process(t.Context())
	require.True(t, executor.request.DryRun)
	require.Empty(t, store.completed)
	require.Empty(t, store.retried)
	require.Equal(t, []uuid.UUID{jobID}, store.deferred)
}

func TestProcessDefersWhileSnapshotAssignmentIsInProgress(t *testing.T) {
	jobID, candidate := uuid.New(), uuid.New()
	store := &fakeStore{
		jobs:       []queries.SnapshotGcJob{{ID: jobID, SandboxID: "sandbox-a", CandidateBuildID: candidate, Attempts: 1}},
		inProgress: true,
	}
	executor := &fakeExecutor{}
	w := New(store, executor, nil, "test")
	w.withSnapshotLock = func(ctx context.Context, _ string, fn func(Store) error) error { return fn(store) }
	w.deleteEnabled = func(context.Context) bool { return true }
	w.process(t.Context())

	require.Empty(t, executor.request.Candidates, "GC must not mutate while a successor header is unavailable")
	require.Equal(t, []uuid.UUID{jobID}, store.deferred)
	require.Empty(t, store.completed)
}

func TestProcessDoesNotMutateAfterLeaseOwnershipIsLost(t *testing.T) {
	jobID, candidate, head := uuid.New(), uuid.New(), uuid.New()
	store := &fakeStore{
		jobs:      []queries.SnapshotGcJob{{ID: jobID, SandboxID: "sandbox-a", CandidateBuildID: candidate, Attempts: 1}},
		heads:     []queries.ListCurrentSnapshotGCHeadsRow{{EnvID: "env-a", BuildID: head}},
		leaseLost: true,
	}
	executorCalled := false
	w := New(store, executorFunc(func(context.Context, ExecuteRequest) (ExecuteResult, error) {
		executorCalled = true
		return ExecuteResult{}, nil
	}), nil, "test")
	w.withSnapshotLock = func(ctx context.Context, _ string, fn func(Store) error) error { return fn(store) }
	w.deleteEnabled = func(context.Context) bool { return true }
	w.process(t.Context())

	require.False(t, executorCalled, "an expired worker must never reach the irreversible storage mutation")
	require.Empty(t, store.completed)
	require.Equal(t, []uuid.UUID{jobID}, store.retried)
}

func TestProcessDoesNotMutateWhenLeaseRenewalLosesRace(t *testing.T) {
	jobID, candidate, head := uuid.New(), uuid.New(), uuid.New()
	store := &fakeStore{
		jobs:      []queries.SnapshotGcJob{{ID: jobID, SandboxID: "sandbox-a", CandidateBuildID: candidate, Attempts: 1}},
		heads:     []queries.ListCurrentSnapshotGCHeadsRow{{EnvID: "env-a", BuildID: head}},
		renewLost: true,
	}
	executorCalled := false
	w := New(store, executorFunc(func(context.Context, ExecuteRequest) (ExecuteResult, error) {
		executorCalled = true
		return ExecuteResult{}, nil
	}), nil, "test")
	w.withSnapshotLock = func(ctx context.Context, _ string, fn func(Store) error) error { return fn(store) }
	w.deleteEnabled = func(context.Context) bool { return true }
	w.process(t.Context())

	require.False(t, executorCalled, "a worker that cannot fence its lease row must never mutate storage")
	require.Empty(t, store.completed)
	require.Equal(t, []uuid.UUID{jobID}, store.retried)
}

func TestDefaultGracePeriodIsFifteenMinutes(t *testing.T) {
	require.Equal(t, 15*time.Minute, DefaultGracePeriod)
	store := &fakeStore{}
	w := New(store, nil, nil, "test")
	require.NoError(t, w.Enqueue(t.Context(), "env-a", uuid.New()))
	require.Equal(t, 15*time.Minute, store.enqueuedGrace)
}

func TestProcessDispatchesClaimedBatchWithBoundedConcurrency(t *testing.T) {
	head := uuid.New()
	jobs := make([]queries.SnapshotGcJob, defaultBatch)
	for i := range jobs {
		jobs[i] = queries.SnapshotGcJob{ID: uuid.New(), SandboxID: uuid.NewString(), CandidateBuildID: uuid.New(), Attempts: 1}
	}
	store := &fakeStore{jobs: jobs, heads: []queries.ListCurrentSnapshotGCHeadsRow{{EnvID: "env-a", BuildID: head}}}

	var active, maximum atomic.Int32
	entered := make(chan struct{}, defaultBatch)
	release := make(chan struct{})
	executor := executorFunc(func(_ context.Context, req ExecuteRequest) (ExecuteResult, error) {
		current := active.Add(1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		active.Add(-1)
		return ExecuteResult{Deleted: map[uuid.UUID]bool{req.Candidates[0].BuildID: true}}, nil
	})
	w := New(store, executor, nil, "test")
	// The production callback serializes final mutations with PostgreSQL. This
	// pass-through test isolates and proves the worker's bounded dispatcher.
	w.withSnapshotLock = func(ctx context.Context, _ string, fn func(Store) error) error { return fn(store) }
	w.deleteEnabled = func(context.Context) bool { return true }
	done := make(chan struct{})
	go func() {
		w.process(t.Context())
		close(done)
	}()
	for range defaultBatch {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("claimed GC batch was not dispatched concurrently")
		}
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("GC batch did not finish")
	}

	require.Equal(t, defaultBatch, maximum.Load())
	require.Equal(t, defaultBatch, store.claimBatch)
	require.Len(t, store.completed, int(defaultBatch))
}
