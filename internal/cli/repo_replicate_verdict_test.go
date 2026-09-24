package cli

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// M26: the exit code and the text verdict of `repo replicate` must agree
// and must count EVERY failure class. The exit ignored manifest replica
// sidecar failures; the text said "✓ replication clean" with WAL
// manifest / WAL aux failures.
func TestRepoReplicate_VerdictCountsEveryFailureClass(t *testing.T) {
	for name, res := range map[string]repo.ReplicateResult{
		"manifest_replicas_failed": {ManifestReplicasFailed: 1},
		"wal_manifests_failed":     {WALManifestsFailed: 1, IncludeWAL: true},
		"wal_aux_failed":           {WALAuxFailed: 1, IncludeWAL: true},
		"manifests_failed":         {ManifestsFailed: 1},
		"chunks_missing":           {ChunksMissing: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if res.Clean() {
				t.Fatalf("%s: Clean() = true; `repo replicate` would exit 0", name)
			}
			var b strings.Builder
			if err := (repoReplicateBody{ReplicateResult: res}).WriteText(&b); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(b.String(), "replication clean") {
				t.Errorf("%s: text says clean:\n%s", name, b.String())
			}
		})
	}
	var ok repo.ReplicateResult
	if !ok.Clean() {
		t.Error("a zero-failure result must be clean")
	}
}
