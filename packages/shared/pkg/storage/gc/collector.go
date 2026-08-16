package storagegc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// Snapshot identifies a current, remotely durable snapshot head. Callers must
// pass every current head whose references share the candidate namespace.
type Snapshot struct {
	BuildID        uuid.UUID
	FilesystemOnly bool
}

// CollectRequest is deliberately explicit: this package never discovers old
// database assignments or decides the grace period itself.
type CollectRequest struct {
	Heads                   []Snapshot
	Candidates              []Candidate
	GlobalProtectedBuildIDs []uuid.UUID
	DryRun                  bool
	Now                     time.Time
}

type CollectResult struct {
	Plan   Plan
	Report Report
}

type Collector struct{ artifacts artifactStore }

func NewCollector(provider storage.StorageProvider) *Collector {
	return &Collector{artifacts: providerArtifacts{provider: provider}}
}

type artifactStore interface {
	LoadHeader(context.Context, string) (*header.Header, error)
	Exists(context.Context, string) (bool, error)
	DeleteObjectsWithPrefix(context.Context, string) error
}

// Collect verifies all current heads before planning or performing a delete.
// Invalid, missing, legacy, or partially uploaded heads fail closed.
func (c *Collector) Collect(ctx context.Context, req CollectRequest) (CollectResult, error) {
	if len(req.Heads) == 0 {
		return CollectResult{}, errors.New("at least one current snapshot head is required")
	}

	heads := make([]Head, 0, len(req.Heads))
	for _, snapshot := range req.Heads {
		head, err := c.loadHead(ctx, snapshot)
		if err != nil {
			return CollectResult{}, err
		}
		heads = append(heads, head)
	}

	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	plan, err := BuildPlanWithProtection(now, heads, req.Candidates, req.GlobalProtectedBuildIDs)
	if err != nil {
		return CollectResult{}, err
	}

	return CollectResult{Plan: plan, Report: Execute(ctx, c.artifacts, plan, req.DryRun)}, nil
}

func (c *Collector) loadHead(ctx context.Context, snapshot Snapshot) (Head, error) {
	if snapshot.BuildID == uuid.Nil {
		return Head{}, errors.New("snapshot head has nil build ID")
	}

	paths := storage.Paths{BuildID: snapshot.BuildID.String()}
	rootfs, err := c.artifacts.LoadHeader(ctx, paths.RootfsHeader())
	if err != nil {
		return Head{}, fmt.Errorf("load snapshot head %s rootfs header: %w", snapshot.BuildID, err)
	}
	if err := requireArtifact(ctx, c.artifacts, paths.Metadata()); err != nil {
		return Head{}, fmt.Errorf("verify snapshot head %s metadata: %w", snapshot.BuildID, err)
	}

	var memory *header.Header
	if !snapshot.FilesystemOnly {
		memory, err = c.artifacts.LoadHeader(ctx, paths.MemfileHeader())
		if err != nil {
			return Head{}, fmt.Errorf("load snapshot head %s memfile header: %w", snapshot.BuildID, err)
		}
		if err := requireArtifact(ctx, c.artifacts, paths.Snapfile()); err != nil {
			return Head{}, fmt.Errorf("verify snapshot head %s snapfile: %w", snapshot.BuildID, err)
		}
	}

	return Head{BuildID: snapshot.BuildID, Memory: memory, Rootfs: rootfs, FilesystemOnly: snapshot.FilesystemOnly}, nil
}

func requireArtifact(ctx context.Context, artifacts artifactStore, path string) error {
	exists, err := artifacts.Exists(ctx, path)
	if err != nil {
		return err
	}
	if !exists {
		return storage.ErrObjectNotExist
	}
	return nil
}

type providerArtifacts struct{ provider storage.StorageProvider }

func (p providerArtifacts) LoadHeader(ctx context.Context, path string) (*header.Header, error) {
	h, _, err := header.LoadHeader(ctx, p.provider, path)
	return h, err
}

func (p providerArtifacts) Exists(ctx context.Context, path string) (bool, error) {
	blob, err := p.provider.OpenBlob(ctx, path)
	if err != nil {
		return false, err
	}
	return blob.Exists(ctx)
}

func (p providerArtifacts) DeleteObjectsWithPrefix(ctx context.Context, prefix string) error {
	return p.provider.DeleteObjectsWithPrefix(ctx, prefix)
}
