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

// Head is a remotely durable snapshot generation. Rootfs is always required;
// Memory is required unless FilesystemOnly is true.
type Head struct {
	BuildID        uuid.UUID
	Memory         *header.Header
	Rootfs         *header.Header
	FilesystemOnly bool
}

// Candidate is an older build prefix considered for collection. NotBefore is
// the end of its rollback grace period.
type Candidate struct {
	BuildID   uuid.UUID
	NotBefore time.Time
}

type RetainReason string

const (
	RetainReachable RetainReason = "reachable"
	RetainGrace     RetainReason = "grace_period"
)

// Plan is a side-effect-free collection decision. Delete contains only build
// prefixes that are unreachable from every verified head and outside grace.
type Plan struct {
	Delete []uuid.UUID
	Retain map[uuid.UUID]RetainReason
}

// BuildPlan validates all live heads before returning any deletion. This
// deliberately fails closed: one missing, incomplete, mismatched, or corrupt
// header makes the entire plan unusable.
func BuildPlan(now time.Time, heads []Head, candidates []Candidate) (Plan, error) {
	return BuildPlanWithProtection(now, heads, candidates, nil)
}

// BuildPlanWithProtection also accepts build IDs protected by verified heads
// outside the caller's local batch (for example, a fork in another snapshot
// env). This explicit global set closes the cross-head deletion race without
// requiring this pure policy package to own database discovery.
func BuildPlanWithProtection(now time.Time, heads []Head, candidates []Candidate, globalProtectedBuildIDs []uuid.UUID) (Plan, error) {
	reachable := make(map[uuid.UUID]struct{})
	for _, buildID := range globalProtectedBuildIDs {
		if buildID == uuid.Nil {
			continue
		}
		reachable[buildID] = struct{}{}
	}
	for _, head := range heads {
		if head.BuildID == uuid.Nil {
			return Plan{}, errors.New("snapshot head has nil build ID")
		}

		reachable[head.BuildID] = struct{}{}
		if !head.FilesystemOnly {
			if err := addHeaderReachability(reachable, head.BuildID, "memfile", head.Memory); err != nil {
				return Plan{}, err
			}
		}
		if err := addHeaderReachability(reachable, head.BuildID, "rootfs", head.Rootfs); err != nil {
			return Plan{}, err
		}
	}

	plan := Plan{Retain: make(map[uuid.UUID]RetainReason)}
	seen := make(map[uuid.UUID]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate.BuildID == uuid.Nil {
			return Plan{}, errors.New("collection candidate has nil build ID")
		}
		if _, duplicate := seen[candidate.BuildID]; duplicate {
			continue
		}
		seen[candidate.BuildID] = struct{}{}

		if _, live := reachable[candidate.BuildID]; live {
			plan.Retain[candidate.BuildID] = RetainReachable
			continue
		}
		if now.Before(candidate.NotBefore) {
			plan.Retain[candidate.BuildID] = RetainGrace
			continue
		}

		plan.Delete = append(plan.Delete, candidate.BuildID)
	}

	return plan, nil
}

func addHeaderReachability(dst map[uuid.UUID]struct{}, headID uuid.UUID, artifact string, h *header.Header) error {
	if h == nil {
		return fmt.Errorf("snapshot head %s is missing %s header", headID, artifact)
	}
	if h.IncompletePendingUpload {
		return fmt.Errorf("snapshot head %s has incomplete %s header", headID, artifact)
	}
	if h.Metadata == nil || h.Metadata.BuildId != headID {
		return fmt.Errorf("snapshot head %s has mismatched %s header", headID, artifact)
	}
	if err := header.ValidateHeader(h); err != nil {
		return fmt.Errorf("snapshot head %s has invalid %s header: %w", headID, artifact, err)
	}
	// V3 headers predate the auxiliary Builds index, but their block mapping is
	// still the authoritative description of which build prefixes contain the
	// data needed by this artifact. Protect every mapped build plus BaseBuildId.
	// For V4+ retain the stronger mapping-to-index consistency check.
	if h.Metadata.Version >= header.MetadataVersionV4 && h.Builds == nil {
		return fmt.Errorf("snapshot head %s has missing %s build index", headID, artifact)
	}
	if h.Metadata.BaseBuildId != uuid.Nil {
		dst[h.Metadata.BaseBuildId] = struct{}{}
	}

	for _, buildID := range h.Mapping.Builds() {
		if buildID == uuid.Nil {
			continue
		}
		if h.Metadata.Version >= header.MetadataVersionV4 {
			if _, ok := h.Builds[buildID]; !ok {
				return fmt.Errorf("snapshot head %s %s mapping references build %s without build metadata", headID, artifact, buildID)
			}
		}
		dst[buildID] = struct{}{}
	}

	return nil
}

type PrefixDeleter interface {
	DeleteObjectsWithPrefix(ctx context.Context, prefix string) error
}

type DeleteFailure struct {
	BuildID uuid.UUID
	Err     error
}

type Report struct {
	Deleted  []uuid.UUID
	Failures []DeleteFailure
}

// Execute deletes every planned prefix independently. It keeps going after a
// partial failure so a later run can retry only the failed/idempotent prefixes.
// Dry-run performs no mutations and reports no object as deleted.
func Execute(ctx context.Context, deleter PrefixDeleter, plan Plan, dryRun bool) Report {
	var report Report
	if dryRun {
		return report
	}

	for _, buildID := range plan.Delete {
		if err := deleter.DeleteObjectsWithPrefix(ctx, buildID.String()); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
			report.Failures = append(report.Failures, DeleteFailure{BuildID: buildID, Err: err})
			continue
		}
		report.Deleted = append(report.Deleted, buildID)
	}

	return report
}
