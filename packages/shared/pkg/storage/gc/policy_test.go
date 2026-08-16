package storagegc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

func testHeader(t *testing.T, head uuid.UUID, refs ...uuid.UUID) *header.Header {
	t.Helper()
	const blockSize = uint64(header.PageSize)
	mappings := make([]header.BuildMap, 0, len(refs))
	for i, ref := range refs {
		mappings = append(mappings, header.BuildMap{
			Offset:             uint64(i) * blockSize,
			Length:             blockSize,
			BuildId:            ref,
			BuildStorageOffset: uint64(i) * blockSize,
		})
	}
	h, err := header.NewHeader(&header.Metadata{
		Version:   header.MetadataVersionV5,
		BlockSize: blockSize,
		Size:      uint64(len(refs)) * blockSize,
		BuildId:   head,
	}, mappings)
	require.NoError(t, err)
	for _, ref := range refs {
		if ref != uuid.Nil {
			h.SetBuild(ref, header.BuildData{})
		}
	}
	return h
}

func TestBuildPlanUsesBothHeadersAndProtectsCrossHeadReferences(t *testing.T) {
	t.Parallel()
	now := time.Now()
	headA, headB := uuid.New(), uuid.New()
	memAncestor, rootAncestor, sharedAncestor, dead := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	plan, err := BuildPlan(now, []Head{
		{BuildID: headA, Memory: testHeader(t, headA, memAncestor, sharedAncestor), Rootfs: testHeader(t, headA, rootAncestor)},
		{BuildID: headB, Memory: testHeader(t, headB, headB), Rootfs: testHeader(t, headB, sharedAncestor)},
	}, []Candidate{
		{BuildID: memAncestor}, {BuildID: rootAncestor}, {BuildID: sharedAncestor},
		{BuildID: headA}, {BuildID: headB}, {BuildID: dead},
	})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{dead}, plan.Delete)
	for _, protected := range []uuid.UUID{memAncestor, rootAncestor, sharedAncestor, headA, headB} {
		require.Equal(t, RetainReachable, plan.Retain[protected])
	}
}

func TestBuildPlanHonorsExplicitGlobalProtection(t *testing.T) {
	t.Parallel()
	head, referencedByOtherHead, dead := uuid.New(), uuid.New(), uuid.New()
	plan, err := BuildPlanWithProtection(time.Now(), []Head{{
		BuildID: head, Memory: testHeader(t, head, head), Rootfs: testHeader(t, head, head),
	}}, []Candidate{{BuildID: referencedByOtherHead}, {BuildID: dead}}, []uuid.UUID{uuid.Nil, referencedByOtherHead})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{dead}, plan.Delete)
	require.Equal(t, RetainReachable, plan.Retain[referencedByOtherHead])
}

func TestBuildPlanProtectsUnmappedBaseBuild(t *testing.T) {
	t.Parallel()
	head, base, dead := uuid.New(), uuid.New(), uuid.New()
	memory := testHeader(t, head, head)
	memory.Metadata.BaseBuildId = base
	plan, err := BuildPlan(time.Now(), []Head{{
		BuildID: head, Memory: memory, Rootfs: testHeader(t, head, head),
	}}, []Candidate{{BuildID: base}, {BuildID: dead}})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{dead}, plan.Delete)
	require.Equal(t, RetainReachable, plan.Retain[base])
}

func TestBuildPlanFilesystemOnlyDoesNotRequireMemoryAndExcludesNil(t *testing.T) {
	t.Parallel()
	head, ancestor, dead := uuid.New(), uuid.New(), uuid.New()
	plan, err := BuildPlan(time.Now(), []Head{{
		BuildID: head, FilesystemOnly: true,
		Rootfs: testHeader(t, head, uuid.Nil, ancestor),
	}}, []Candidate{{BuildID: ancestor}, {BuildID: dead}})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{dead}, plan.Delete)
	require.Equal(t, RetainReachable, plan.Retain[ancestor])
}

func TestBuildPlanHonorsGraceAndDeduplicatesCandidates(t *testing.T) {
	t.Parallel()
	now := time.Now()
	head, grace, expired := uuid.New(), uuid.New(), uuid.New()
	plan, err := BuildPlan(now, []Head{{
		BuildID: head, Memory: testHeader(t, head, head), Rootfs: testHeader(t, head, head),
	}}, []Candidate{
		{BuildID: grace, NotBefore: now.Add(time.Minute)},
		{BuildID: expired, NotBefore: now.Add(-time.Minute)},
		{BuildID: expired, NotBefore: now.Add(-time.Minute)},
	})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{expired}, plan.Delete)
	require.Equal(t, RetainGrace, plan.Retain[grace])
}

func TestBuildPlanFailsClosedForMissingIncompleteMismatchedOrCorruptHeaders(t *testing.T) {
	t.Parallel()
	head := uuid.New()
	valid := testHeader(t, head, head)
	incomplete := testHeader(t, head, head)
	incomplete.IncompletePendingUpload = true
	mismatched := testHeader(t, uuid.New(), uuid.New())
	corrupt := testHeader(t, head, head)
	corrupt.Metadata.Size += header.PageSize

	tests := []Head{
		{BuildID: head, Memory: nil, Rootfs: valid},
		{BuildID: head, Memory: incomplete, Rootfs: valid},
		{BuildID: head, Memory: mismatched, Rootfs: valid},
		{BuildID: head, Memory: corrupt, Rootfs: valid},
		{BuildID: head, Memory: valid, Rootfs: nil},
	}
	for _, h := range tests {
		plan, err := BuildPlan(time.Now(), []Head{h}, []Candidate{{BuildID: uuid.New()}})
		require.Error(t, err)
		require.Empty(t, plan.Delete)
	}
}

func TestBuildPlanFailsClosedForLegacyOrUnindexedHeaders(t *testing.T) {
	t.Parallel()
	head := uuid.New()
	root := testHeader(t, head, head)

	legacy := testHeader(t, head, head)
	legacy.Metadata.Version = 3
	plan, err := BuildPlan(time.Now(), []Head{{BuildID: head, Memory: legacy, Rootfs: root}}, []Candidate{{BuildID: uuid.New()}})
	require.Error(t, err)
	require.Empty(t, plan.Delete)

	unindexed := testHeader(t, head, head)
	unindexed.Builds = nil
	plan, err = BuildPlan(time.Now(), []Head{{BuildID: head, Memory: unindexed, Rootfs: root}}, []Candidate{{BuildID: uuid.New()}})
	require.Error(t, err)
	require.Empty(t, plan.Delete)
}

type recordingDeleter struct {
	calls []string
	fail  map[string]error
}

func (d *recordingDeleter) DeleteObjectsWithPrefix(_ context.Context, prefix string) error {
	d.calls = append(d.calls, prefix)
	return d.fail[prefix]
}

func TestExecuteDryRunAndPartialRetry(t *testing.T) {
	t.Parallel()
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	plan := Plan{Delete: []uuid.UUID{a, b, c}}
	d := &recordingDeleter{fail: map[string]error{b.String(): errors.New("transient")}}

	require.Empty(t, Execute(t.Context(), d, plan, true).Deleted)
	require.Empty(t, d.calls)

	report := Execute(t.Context(), d, plan, false)
	require.Equal(t, []string{a.String(), b.String(), c.String()}, d.calls)
	require.Equal(t, []uuid.UUID{a, c}, report.Deleted)
	require.Len(t, report.Failures, 1)
	require.Equal(t, b, report.Failures[0].BuildID)

	// Retrying an independently failed prefix doesn't repeat successful work.
	d.fail = nil
	retry := Execute(t.Context(), d, Plan{Delete: []uuid.UUID{b}}, false)
	require.Equal(t, []uuid.UUID{b}, retry.Deleted)
	require.Empty(t, retry.Failures)
}
