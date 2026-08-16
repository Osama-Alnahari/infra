package snapshotgc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	sqlcdb "github.com/e2b-dev/infra/packages/db/client"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

const (
	DefaultGracePeriod = 15 * time.Minute
	defaultLease       = 30 * time.Minute
	defaultPoll        = 30 * time.Second
	defaultReconcile   = 10 * time.Minute
	// A batch is dispatched concurrently, but every candidate still executes in
	// its own global+sandbox fenced transaction. The global fence serializes the
	// final proof/mutation while concurrent dispatch removes the 30s poll gap
	// between candidates and overlaps lock acquisition/coordination safely.
	defaultBatch       int32 = 4
	defaultMaxAttempts int32 = 12
)

type Store interface {
	EnqueueSupersededSnapshotGCJobs(context.Context, queries.EnqueueSupersededSnapshotGCJobsParams) (int64, error)
	ReconcileSnapshotGCJobs(context.Context, queries.ReconcileSnapshotGCJobsParams) (int64, error)
	ClaimSnapshotGCJobs(context.Context, queries.ClaimSnapshotGCJobsParams) ([]queries.SnapshotGcJob, error)
	CompleteSnapshotGCJob(context.Context, queries.CompleteSnapshotGCJobParams) (int64, error)
	OwnsSnapshotGCLease(context.Context, queries.OwnsSnapshotGCLeaseParams) (bool, error)
	RenewSnapshotGCLease(context.Context, queries.RenewSnapshotGCLeaseParams) (int64, error)
	RetrySnapshotGCJob(context.Context, queries.RetrySnapshotGCJobParams) (int64, error)
	DeferSnapshotGCJob(context.Context, queries.DeferSnapshotGCJobParams) (int64, error)
	ListSnapshotGCProtectedBuilds(context.Context) ([]uuid.UUID, error)
	ListCurrentSnapshotGCHeads(context.Context) ([]queries.ListCurrentSnapshotGCHeadsRow, error)
	HasInProgressSnapshotBuilds(context.Context) (bool, error)
}

type Head struct {
	BuildID        uuid.UUID
	FilesystemOnly bool
}
type Candidate struct {
	BuildID   uuid.UUID
	NotBefore time.Time
}
type ExecuteRequest struct {
	Heads                   []Head
	Candidates              []Candidate
	GlobalProtectedBuildIDs []uuid.UUID
	DryRun                  bool
}
type ExecuteResult struct {
	Deleted  map[uuid.UUID]bool
	Retained map[uuid.UUID]bool
}
type Executor interface {
	ExecuteSnapshotGC(context.Context, ExecuteRequest) (ExecuteResult, error)
}

type Worker struct {
	store            Store
	executor         Executor
	executorMu       sync.RWMutex
	flags            *featureflags.Client
	owner            string
	grace            time.Duration
	now              func() time.Time
	deleteEnabled    func(context.Context) bool
	withSnapshotLock func(context.Context, string, func(Store) error) error
}

func New(store Store, executor Executor, flags *featureflags.Client, owner string) *Worker {
	w := &Worker{store: store, executor: executor, flags: flags, owner: owner, grace: DefaultGracePeriod, now: time.Now}
	w.deleteEnabled = func(ctx context.Context) bool {
		return flags != nil && flags.BoolFlag(ctx, featureflags.SnapshotGCDeleteEnabledFlag)
	}
	if db, ok := store.(*sqlcdb.Client); ok {
		w.withSnapshotLock = func(ctx context.Context, sandboxID string, fn func(Store) error) error {
			return db.WithSnapshotGCLock(ctx, sandboxID, func(locked *sqlcdb.Client) error { return fn(locked) })
		}
	}
	return w
}

func (w *Worker) SetExecutor(executor Executor) {
	w.executorMu.Lock()
	w.executor = executor
	w.executorMu.Unlock()
}
func (w *Worker) getExecutor() Executor {
	w.executorMu.RLock()
	defer w.executorMu.RUnlock()
	return w.executor
}

func (w *Worker) Enqueue(ctx context.Context, envID string, successor uuid.UUID) error {
	_, err := w.store.EnqueueSupersededSnapshotGCJobs(ctx, queries.EnqueueSupersededSnapshotGCJobsParams{SnapshotEnvID: envID, SuccessorBuildID: &successor, GracePeriod: interval(w.grace)})
	return err
}

func interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

func (w *Worker) Run(ctx context.Context) {
	poll := time.NewTicker(defaultPoll)
	defer poll.Stop()
	reconcile := time.NewTicker(defaultReconcile)
	defer reconcile.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-reconcile.C:
			if w.enabled(ctx) {
				w.reconcile(ctx)
			}
		case <-poll.C:
			if w.enabled(ctx) {
				w.process(ctx)
			}
		}
	}
}
func (w *Worker) enabled(ctx context.Context) bool {
	return w.flags != nil && w.flags.BoolFlag(ctx, featureflags.SnapshotGCEnabledFlag)
}
func (w *Worker) reconcile(ctx context.Context) {
	n, e := w.store.ReconcileSnapshotGCJobs(ctx, queries.ReconcileSnapshotGCJobsParams{BatchSize: 256, GracePeriod: interval(w.grace)})
	if e != nil {
		logger.L().Error(ctx, "snapshot GC reconciliation failed", zap.Error(e))
		return
	}
	if n > 0 {
		logger.L().Info(ctx, "snapshot GC jobs reconciled", zap.Int64("count", n))
	}
}

func (w *Worker) process(ctx context.Context) {
	executor := w.getExecutor()
	if executor == nil {
		logger.L().Warn(ctx, "snapshot GC enabled without executor")
		return
	}
	jobs, err := w.store.ClaimSnapshotGCJobs(ctx, queries.ClaimSnapshotGCJobsParams{BatchSize: defaultBatch, LeaseOwner: &w.owner, LeaseDuration: interval(defaultLease)})
	if err != nil {
		logger.L().Error(ctx, "claim snapshot GC jobs", zap.Error(err))
		return
	}
	if len(jobs) == 0 {
		return
	}
	if w.withSnapshotLock == nil {
		w.retryAll(ctx, jobs, errors.New("snapshot GC has no advisory-lock provider"))
		return
	}

	var wg sync.WaitGroup
	for _, job := range jobs {
		job := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			if jobErr := w.processJob(ctx, executor, job); jobErr != nil {
				w.retry(ctx, job, jobErr)
			}
		}()
	}
	wg.Wait()
}

func (w *Worker) processJob(ctx context.Context, executor Executor, job queries.SnapshotGcJob) error {
	return w.withSnapshotLock(ctx, job.SandboxID, func(locked Store) error {
		// These are the final reads. The same advisory lock fences pause snapshot
		// assignment creation until the storage mutation and job transition commit.
		unsafe, listErr := locked.HasInProgressSnapshotBuilds(ctx)
		if listErr != nil {
			return fmt.Errorf("check in-progress snapshot builds: %w", listErr)
		}
		if unsafe {
			_, listErr = locked.DeferSnapshotGCJob(ctx, queries.DeferSnapshotGCJobParams{ID: job.ID, LeaseOwner: &w.owner, NextAttemptAt: w.now().Add(time.Minute), LastError: "in-progress snapshot build prevents safe reachability proof"})
			return listErr
		}
		heads, listErr := locked.ListCurrentSnapshotGCHeads(ctx)
		if listErr != nil {
			return fmt.Errorf("list current snapshot heads: %w", listErr)
		}
		protected, listErr := locked.ListSnapshotGCProtectedBuilds(ctx)
		if listErr != nil {
			return fmt.Errorf("list global protected builds: %w", listErr)
		}
		req := ExecuteRequest{GlobalProtectedBuildIDs: protected, DryRun: !w.deleteEnabled(ctx), Candidates: []Candidate{{BuildID: job.CandidateBuildID, NotBefore: job.NotBefore}}}
		for _, h := range heads {
			req.Heads = append(req.Heads, Head{BuildID: h.BuildID, FilesystemOnly: h.FilesystemOnly})
		}
		ownsLease, leaseErr := locked.OwnsSnapshotGCLease(ctx, queries.OwnsSnapshotGCLeaseParams{ID: job.ID, LeaseOwner: &w.owner})
		if leaseErr != nil {
			return fmt.Errorf("verify snapshot GC lease ownership: %w", leaseErr)
		}
		if !ownsLease {
			return errors.New("snapshot GC lease expired or ownership was lost before storage mutation")
		}
		renewed, leaseErr := locked.RenewSnapshotGCLease(ctx, queries.RenewSnapshotGCLeaseParams{ID: job.ID, LeaseOwner: &w.owner, LeaseDuration: interval(defaultLease)})
		if leaseErr != nil {
			return fmt.Errorf("renew snapshot GC lease before storage mutation: %w", leaseErr)
		}
		if renewed != 1 {
			return errors.New("snapshot GC lease expired or ownership was lost before renewal")
		}
		result, executeErr := executor.ExecuteSnapshotGC(ctx, req)
		if executeErr != nil {
			return executeErr
		}
		if req.DryRun || result.Retained[job.CandidateBuildID] {
			_, executeErr = locked.DeferSnapshotGCJob(ctx, queries.DeferSnapshotGCJobParams{ID: job.ID, LeaseOwner: &w.owner, NextAttemptAt: w.now().Add(time.Hour), LastError: "snapshot retained by dry-run, reachability, or grace policy"})
			return executeErr
		}
		if !result.Deleted[job.CandidateBuildID] {
			return errors.New("snapshot GC executor returned no terminal outcome")
		}
		updated, executeErr := locked.CompleteSnapshotGCJob(ctx, queries.CompleteSnapshotGCJobParams{ID: job.ID, LeaseOwner: &w.owner})
		if executeErr == nil && updated != 1 {
			return errors.New("snapshot GC lease ownership was lost before completion")
		}
		return executeErr
	})
}
func (w *Worker) retryAll(ctx context.Context, jobs []queries.SnapshotGcJob, err error) {
	for _, j := range jobs {
		w.retry(ctx, j, err)
	}
}
func (w *Worker) retry(ctx context.Context, j queries.SnapshotGcJob, err error) {
	power := math.Min(float64(j.Attempts), 8)
	delay := time.Minute * time.Duration(math.Pow(2, power))
	_, e := w.store.RetrySnapshotGCJob(ctx, queries.RetrySnapshotGCJobParams{ID: j.ID, LeaseOwner: &w.owner, MaxAttempts: defaultMaxAttempts, NextAttemptAt: w.now().Add(delay), LastError: err.Error()})
	if e != nil {
		logger.L().Error(ctx, "retry snapshot GC job", zap.Error(e), zap.String("job_id", j.ID.String()))
	}
}
func (w *Worker) deferJob(ctx context.Context, j queries.SnapshotGcJob, reason string) {
	_, e := w.store.DeferSnapshotGCJob(ctx, queries.DeferSnapshotGCJobParams{ID: j.ID, LeaseOwner: &w.owner, NextAttemptAt: w.now().Add(time.Hour), LastError: reason})
	if e != nil {
		logger.L().Error(ctx, "defer snapshot GC job", zap.Error(e), zap.String("job_id", j.ID.String()))
	}
}
