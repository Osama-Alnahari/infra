//go:build linux

package server

import (
	"context"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
	storagegc "github.com/e2b-dev/infra/packages/shared/pkg/storage/gc"
)

// SnapshotGC exposes the dependency-aware collector on the existing protected
// template-manager control plane. Candidate/global protection discovery stays
// in the API, while this storage owner performs final artifact verification.
func (s *ServerStore) SnapshotGC(ctx context.Context, in *templatemanager.SnapshotGCRequest) (*templatemanager.SnapshotGCResponse, error) {
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "snapshot GC request is required")
	}

	heads := make([]storagegc.Snapshot, 0, len(in.GetHeads()))
	for _, input := range in.GetHeads() {
		buildID, err := parseSnapshotGCBuildID(input.GetBuildId(), "head")
		if err != nil {
			return nil, err
		}
		heads = append(heads, storagegc.Snapshot{BuildID: buildID, FilesystemOnly: input.GetFilesystemOnly()})
	}

	candidates := make([]storagegc.Candidate, 0, len(in.GetCandidates()))
	for _, input := range in.GetCandidates() {
		buildID, err := parseSnapshotGCBuildID(input.GetBuildId(), "candidate")
		if err != nil {
			return nil, err
		}
		if input.GetNotBefore() == nil || !input.GetNotBefore().IsValid() {
			return nil, status.Errorf(codes.InvalidArgument, "candidate %s has invalid not_before", buildID)
		}
		candidates = append(candidates, storagegc.Candidate{BuildID: buildID, NotBefore: input.GetNotBefore().AsTime()})
	}

	protected := make([]uuid.UUID, 0, len(in.GetGlobalProtectedBuildIds()))
	for _, raw := range in.GetGlobalProtectedBuildIds() {
		buildID, err := parseSnapshotGCBuildID(raw, "globally protected")
		if err != nil {
			return nil, err
		}
		protected = append(protected, buildID)
	}

	result, err := storagegc.NewCollector(s.templateStorage).Collect(ctx, storagegc.CollectRequest{
		Heads: heads, Candidates: candidates, GlobalProtectedBuildIDs: protected, DryRun: in.GetDryRun(),
	})
	if err != nil {
		// Verification errors are failed preconditions: no prefix was touched.
		return nil, status.Errorf(codes.FailedPrecondition, "snapshot GC verification failed: %v", err)
	}

	outcomes := make(map[uuid.UUID]*templatemanager.SnapshotGCOutcome, len(candidates))
	for _, candidate := range candidates {
		outcomes[candidate.BuildID] = &templatemanager.SnapshotGCOutcome{BuildId: candidate.BuildID.String()}
	}
	for buildID, reason := range result.Plan.Retain {
		kind := templatemanager.SnapshotGCOutcomeKind_SNAPSHOT_GC_OUTCOME_RETAINED_REACHABLE
		if reason == storagegc.RetainGrace {
			kind = templatemanager.SnapshotGCOutcomeKind_SNAPSHOT_GC_OUTCOME_RETAINED_GRACE
		}
		outcomes[buildID].Outcome = kind
	}
	for _, buildID := range result.Plan.Delete {
		outcomes[buildID].Outcome = templatemanager.SnapshotGCOutcomeKind_SNAPSHOT_GC_OUTCOME_DELETE_PLANNED
	}
	for _, buildID := range result.Report.Deleted {
		outcomes[buildID].Outcome = templatemanager.SnapshotGCOutcomeKind_SNAPSHOT_GC_OUTCOME_DELETED
	}
	for _, failure := range result.Report.Failures {
		outcomes[failure.BuildID].Outcome = templatemanager.SnapshotGCOutcomeKind_SNAPSHOT_GC_OUTCOME_DELETE_FAILED
		outcomes[failure.BuildID].Error = failure.Err.Error()
	}

	response := &templatemanager.SnapshotGCResponse{Outcomes: make([]*templatemanager.SnapshotGCOutcome, 0, len(candidates))}
	seen := make(map[uuid.UUID]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, duplicate := seen[candidate.BuildID]; duplicate {
			continue
		}
		seen[candidate.BuildID] = struct{}{}
		response.Outcomes = append(response.Outcomes, outcomes[candidate.BuildID])
	}

	return response, nil
}

func parseSnapshotGCBuildID(raw, kind string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, status.Errorf(codes.InvalidArgument, "invalid %s build id %q: %v", kind, raw, err)
	}
	if id == uuid.Nil {
		return uuid.Nil, status.Errorf(codes.InvalidArgument, "invalid %s build id %q: nil UUID", kind, raw)
	}
	return id, nil
}
