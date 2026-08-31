package cli

// `repo replicate`'s text output is the verdict an operator acts on.
// Before manifest-replica failures were counted, a run in which every
// sidecar failed printed
//
//	✓ replication clean
//	First 50 failure(s):
//	  manifests/_replicas/... — put dst (best-effort): ...
//
// — a clean verdict directly above fifty failures, with no number
// anywhere saying how many there really were. These tests pin the
// three properties that make the output self-consistent.

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func replicateBodyWith(mut func(*repoReplicateBody)) repoReplicateBody {
	b := repoReplicateBody{ReplicateResult: repo.ReplicateResult{
		ManifestsConsidered: 60, ManifestsCopied: 60,
		ChunksConsidered: 60, ChunksCopied: 60,
	}}
	mut(&b)
	return b
}

func renderReplicate(t *testing.T, b repoReplicateBody) string {
	t.Helper()
	var sb strings.Builder
	if err := b.WriteText(&sb); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return sb.String()
}

// The regression.
func TestRepoReplicateWriteText_ReplicaFailuresAreNotClean(t *testing.T) {
	b := replicateBodyWith(func(b *repoReplicateBody) {
		b.ManifestReplicasFailed = 60
		for i := 0; i < 50; i++ {
			b.Failures = append(b.Failures, repo.ReplicateFailure{
				Key: "manifests/_replicas/x.manifest.json", Err: "put dst (best-effort): nope",
			})
		}
	})
	out := renderReplicate(t, b)

	if strings.Contains(out, "replication clean") {
		t.Errorf("60 failed manifest replicas rendered as a clean replication:\n%s", out)
	}
	if !strings.Contains(out, "60 sidecar(s) not copied") {
		t.Errorf("the replica-failure count must appear:\n%s", out)
	}
	if !strings.Contains(out, "repair manifest") {
		t.Errorf("the line must say what was lost, not just that a number is non-zero:\n%s", out)
	}
}

// The failure list header claimed "First 50 failure(s)" whether or not
// 50 was the total. It must distinguish the two.
func TestRepoReplicateWriteText_TruncatedFailureListStatesTheTotal(t *testing.T) {
	trunc := replicateBodyWith(func(b *repoReplicateBody) {
		b.ManifestReplicasFailed = 60
		for i := 0; i < 50; i++ {
			b.Failures = append(b.Failures, repo.ReplicateFailure{Key: "k", Err: "e"})
		}
	})
	if out := renderReplicate(t, trunc); !strings.Contains(out, "First 50 of 60 failure(s)") {
		t.Errorf("a truncated list must state the true total:\n%s", out)
	}

	whole := replicateBodyWith(func(b *repoReplicateBody) {
		b.ManifestsFailed = 2
		for i := 0; i < 2; i++ {
			b.Failures = append(b.Failures, repo.ReplicateFailure{Key: "k", Err: "e"})
		}
	})
	out := renderReplicate(t, whole)
	if strings.Contains(out, "First") {
		t.Errorf("a complete list must not claim to be a prefix:\n%s", out)
	}
	if !strings.Contains(out, "2 failure(s)") {
		t.Errorf("expected a plain count for a complete list:\n%s", out)
	}
}

// A genuinely clean run must still read as clean, or the new line
// makes every healthy replication look degraded.
func TestRepoReplicateWriteText_CleanRunStillReadsClean(t *testing.T) {
	out := renderReplicate(t, replicateBodyWith(func(*repoReplicateBody) {}))
	if !strings.Contains(out, "✓ replication clean") {
		t.Errorf("a clean run must render as clean:\n%s", out)
	}
	if strings.Contains(out, "sidecar(s) not copied") {
		t.Errorf("the replica line must not appear when nothing failed:\n%s", out)
	}
}
