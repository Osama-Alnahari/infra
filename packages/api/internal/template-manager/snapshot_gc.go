package template_manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	snapshotgcworker "github.com/e2b-dev/infra/packages/api/internal/orchestrator/snapshotgc"
	templatemanagergrpc "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
	"github.com/e2b-dev/infra/packages/shared/pkg/machineinfo"
)

var ErrSnapshotGCExecutorUnavailable = errors.New("snapshot GC executor unavailable")

// ExecuteSnapshotGC sends one globally protected batch to a storage-owning
// template-manager. The server re-verifies every header before mutation.
func (tm *TemplateManager) ExecuteSnapshotGC(ctx context.Context, req snapshotgcworker.ExecuteRequest) (snapshotgcworker.ExecuteResult, error) {
	clusters := tm.clusters.GetClusters()
	// Storage destinations may differ by cluster. Until jobs carry cluster
	// ownership, choosing an arbitrary executor could verify/delete in the wrong
	// bucket, so multi-cluster execution fails closed.
	if len(clusters) != 1 {
		return snapshotgcworker.ExecuteResult{}, fmt.Errorf("%w: expected exactly one storage cluster, got %d", ErrSnapshotGCExecutorUnavailable, len(clusters))
	}
	var client templatemanagergrpc.TemplateServiceClient
	for _, cluster := range clusters {
		instance, err := cluster.GetAvailableTemplateBuilder(ctx, machineinfo.MachineInfo{})
		if err == nil {
			client = instance.GetClient().Template
			break
		}
	}
	if client == nil {
		return snapshotgcworker.ExecuteResult{}, ErrSnapshotGCExecutorUnavailable
	}

	in := &templatemanagergrpc.SnapshotGCRequest{DryRun: req.DryRun, GlobalProtectedBuildIds: make([]string, 0, len(req.GlobalProtectedBuildIDs))}
	for _, h := range req.Heads {
		in.Heads = append(in.Heads, &templatemanagergrpc.SnapshotGCHead{BuildId: h.BuildID.String(), FilesystemOnly: h.FilesystemOnly})
	}
	for _, c := range req.Candidates {
		in.Candidates = append(in.Candidates, &templatemanagergrpc.SnapshotGCCandidate{BuildId: c.BuildID.String(), NotBefore: timestamppb.New(c.NotBefore)})
	}
	for _, id := range req.GlobalProtectedBuildIDs {
		in.GlobalProtectedBuildIds = append(in.GlobalProtectedBuildIds, id.String())
	}
	out, err := client.SnapshotGC(ctx, in)
	if err != nil {
		return snapshotgcworker.ExecuteResult{}, fmt.Errorf("execute snapshot GC: %w", err)
	}
	result := snapshotgcworker.ExecuteResult{Deleted: map[uuid.UUID]bool{}, Retained: map[uuid.UUID]bool{}}
	for _, o := range out.GetOutcomes() {
		id, parseErr := uuid.Parse(o.GetBuildId())
		if parseErr != nil {
			return result, fmt.Errorf("invalid snapshot GC outcome build ID: %w", parseErr)
		}
		switch o.GetOutcome() {
		case templatemanagergrpc.SnapshotGCOutcomeKind_SNAPSHOT_GC_OUTCOME_DELETED:
			result.Deleted[id] = true
		case templatemanagergrpc.SnapshotGCOutcomeKind_SNAPSHOT_GC_OUTCOME_RETAINED_REACHABLE, templatemanagergrpc.SnapshotGCOutcomeKind_SNAPSHOT_GC_OUTCOME_RETAINED_GRACE, templatemanagergrpc.SnapshotGCOutcomeKind_SNAPSHOT_GC_OUTCOME_DELETE_PLANNED:
			result.Retained[id] = true
		case templatemanagergrpc.SnapshotGCOutcomeKind_SNAPSHOT_GC_OUTCOME_DELETE_FAILED:
			return result, fmt.Errorf("snapshot GC delete failed for %s: %s", id, o.GetError())
		}
	}
	return result, nil
}
