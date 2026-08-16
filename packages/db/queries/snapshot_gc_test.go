package queries

import (
	"strings"
	"testing"
)

func TestListCurrentSnapshotGCHeadsUsesPausedSandboxConfigKey(t *testing.T) {
	if !strings.Contains(listCurrentSnapshotGCHeads, "s.config->>'filesystemOnly'") {
		t.Fatal("snapshot GC must read PausedSandboxConfig.filesystemOnly")
	}
	if strings.Contains(listCurrentSnapshotGCHeads, "s.config->>'filesystem_only'") {
		t.Fatal("snapshot GC must not read metadata.json's filesystem_only key")
	}
	if !strings.Contains(listCurrentSnapshotGCHeads, "FROM public.env_build_assignments eba") ||
		!strings.Contains(listCurrentSnapshotGCHeads, "JOIN public.active_envs env") ||
		!strings.Contains(listCurrentSnapshotGCHeads, "LEFT JOIN public.snapshots s") {
		t.Fatal("snapshot GC must verify every active default-assignment head, including non-snapshot heads")
	}
	if strings.Contains(listCurrentSnapshotGCHeads, "env.source = 'snapshot'") {
		t.Fatal("snapshot GC head verification must not be limited to snapshot environments")
	}
}

func TestSnapshotRootWritesUseSharedGlobalFence(t *testing.T) {
	for name, query := range map[string]string{
		"pause snapshot":    upsertSnapshot,
		"snapshot template": createSnapshotTemplateEnv,
		"template tag":      createTemplateBuildAssignment,
	} {
		if !strings.Contains(query, "pg_advisory_xact_lock_shared") {
			t.Fatalf("%s must take the shared snapshot-GC fence", name)
		}
	}
}
