package storagegc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

type fakeArtifacts struct {
	headers   map[string]*header.Header
	exists    map[string]bool
	deleted   []string
	deleteErr map[string]error
}

func (f *fakeArtifacts) LoadHeader(_ context.Context, path string) (*header.Header, error) {
	h, ok := f.headers[path]
	if !ok {
		return nil, storage.ErrObjectNotExist
	}
	return h, nil
}

func (f *fakeArtifacts) Exists(_ context.Context, path string) (bool, error) {
	return f.exists[path], nil
}

func (f *fakeArtifacts) DeleteObjectsWithPrefix(_ context.Context, prefix string) error {
	f.deleted = append(f.deleted, prefix)
	return f.deleteErr[prefix]
}

func fullHeadArtifacts(t *testing.T, buildID uuid.UUID, refs ...uuid.UUID) *fakeArtifacts {
	p := storage.Paths{BuildID: buildID.String()}
	return &fakeArtifacts{
		headers: map[string]*header.Header{
			p.MemfileHeader(): testHeader(t, buildID, refs...),
			p.RootfsHeader():  testHeader(t, buildID, refs...),
		},
		exists:    map[string]bool{p.Metadata(): true, p.Snapfile(): true},
		deleteErr: map[string]error{},
	}
}

func TestCollectorVerifiesArtifactsAndHonorsGlobalProtection(t *testing.T) {
	t.Parallel()
	head, ancestor, globallyProtected, dead := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	artifacts := fullHeadArtifacts(t, head, head, ancestor)
	collector := &Collector{artifacts: artifacts}

	result, err := collector.Collect(t.Context(), CollectRequest{
		Heads:                   []Snapshot{{BuildID: head}},
		Candidates:              []Candidate{{BuildID: ancestor}, {BuildID: globallyProtected}, {BuildID: dead}},
		GlobalProtectedBuildIDs: []uuid.UUID{globallyProtected},
		Now:                     time.Now(),
	})
	require.NoError(t, err)
	require.Equal(t, []string{dead.String()}, artifacts.deleted)
	require.Equal(t, []uuid.UUID{dead}, result.Report.Deleted)
}

func TestCollectorDryRunReportsPlanWithoutDeleting(t *testing.T) {
	t.Parallel()
	head, dead := uuid.New(), uuid.New()
	artifacts := fullHeadArtifacts(t, head, head)
	result, err := (&Collector{artifacts: artifacts}).Collect(t.Context(), CollectRequest{
		Heads: []Snapshot{{BuildID: head}}, Candidates: []Candidate{{BuildID: dead}}, DryRun: true,
	})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{dead}, result.Plan.Delete)
	require.Empty(t, result.Report.Deleted)
	require.Empty(t, artifacts.deleted)
}

func TestCollectorFilesystemOnlyDoesNotRequireMemoryOrSnapfile(t *testing.T) {
	t.Parallel()
	head, dead := uuid.New(), uuid.New()
	p := storage.Paths{BuildID: head.String()}
	artifacts := &fakeArtifacts{
		headers: map[string]*header.Header{p.RootfsHeader(): testHeader(t, head, head)},
		exists:  map[string]bool{p.Metadata(): true}, deleteErr: map[string]error{},
	}
	_, err := (&Collector{artifacts: artifacts}).Collect(t.Context(), CollectRequest{
		Heads: []Snapshot{{BuildID: head, FilesystemOnly: true}}, Candidates: []Candidate{{BuildID: dead}},
	})
	require.NoError(t, err)
	require.Equal(t, []string{dead.String()}, artifacts.deleted)
}

func TestCollectorFailsClosedBeforeDeletionWhenSuccessorIsIncomplete(t *testing.T) {
	t.Parallel()
	head, dead := uuid.New(), uuid.New()
	for name, mutate := range map[string]func(*fakeArtifacts){
		"missing metadata":      func(f *fakeArtifacts) { f.exists[storage.Paths{BuildID: head.String()}.Metadata()] = false },
		"missing snapfile":      func(f *fakeArtifacts) { f.exists[storage.Paths{BuildID: head.String()}.Snapfile()] = false },
		"missing root header":   func(f *fakeArtifacts) { delete(f.headers, storage.Paths{BuildID: head.String()}.RootfsHeader()) },
		"missing memory header": func(f *fakeArtifacts) { delete(f.headers, storage.Paths{BuildID: head.String()}.MemfileHeader()) },
	} {
		t.Run(name, func(t *testing.T) {
			artifacts := fullHeadArtifacts(t, head, head)
			mutate(artifacts)
			_, err := (&Collector{artifacts: artifacts}).Collect(t.Context(), CollectRequest{
				Heads: []Snapshot{{BuildID: head}}, Candidates: []Candidate{{BuildID: dead}},
			})
			require.Error(t, err)
			require.Empty(t, artifacts.deleted)
		})
	}
}

func TestExecuteTreatsMissingPrefixAsIdempotent(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	d := &fakeArtifacts{deleteErr: map[string]error{id.String(): storage.ErrObjectNotExist}}
	report := Execute(t.Context(), d, Plan{Delete: []uuid.UUID{id}}, false)
	require.Equal(t, []uuid.UUID{id}, report.Deleted)
	require.Empty(t, report.Failures)
}

func TestCollectorReturnsDeleteFailureForReconciliation(t *testing.T) {
	t.Parallel()
	head, dead := uuid.New(), uuid.New()
	artifacts := fullHeadArtifacts(t, head, head)
	artifacts.deleteErr[dead.String()] = errors.New("transient")
	result, err := (&Collector{artifacts: artifacts}).Collect(t.Context(), CollectRequest{
		Heads: []Snapshot{{BuildID: head}}, Candidates: []Candidate{{BuildID: dead}},
	})
	require.NoError(t, err)
	require.Len(t, result.Report.Failures, 1)
}
