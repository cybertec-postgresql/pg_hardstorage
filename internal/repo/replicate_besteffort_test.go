package repo_test

// Manifest replica sidecars (manifests/_replicas/<id>.manifest.json)
// are copied best-effort: a failure there is deliberately NOT a
// primary-replication failure, because the primary manifest did land.
//
// It still has to be counted. It was not, and the per-key Failures
// slice is capped at 50, so a run in which every replica sidecar
// failed reported manifests_failed=0 with 50 truncated failure
// entries — while the destination held zero manifest replicas. That is
// the redundancy `repair manifest` recovers a corrupt primary from. An
// operator reading that result would conclude the destination was a
// faithful copy; it was a copy with its entire disaster-recovery
// second layer missing, and the result gave them no number to notice
// it by.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/faultinject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// seedBackupsWithSidecars plants n backups, each with a primary
// manifest and a replica sidecar.
func seedBackupsWithSidecars(t *testing.T, src storage.StoragePlugin, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("db1.full.%03d", i)
		ch := putChunk(t, src, []byte(id))
		putManifest(t, src, "db1", id, []repo.Hash{ch})
		putRaw(t, src, "manifests/_replicas/"+id+".manifest.json",
			[]byte(`{"backup_id":"`+id+`"}`))
	}
}

// failReplicaPuts wraps dst so every write under manifests/_replicas/
// fails, leaving primaries untouched.
func failReplicaPuts(dst storage.StoragePlugin, err error) storage.StoragePlugin {
	m := faultinject.New(dst)
	m.Activate([]faultinject.Rule{{
		Name: "replica-sidecar-put", Ops: faultinject.OpPut,
		KeyPrefix: "manifests/_replicas/", Err: err,
	}}, faultinject.ActivateOptions{})
	return m
}

// The regression: more replica failures than the detail cap holds.
func TestReplicate_ReplicaFailuresAreCountedPastTheDetailCap(t *testing.T) {
	src, dstRaw := twoRepos(t)
	const n = 60 // > maxReplicateFailures (50)
	seedBackupsWithSidecars(t, src, n)

	res, err := repo.Replicate(context.Background(), src,
		failReplicaPuts(dstRaw, errors.New("dst rejected")), repo.ReplicateOptions{})
	if err != nil {
		t.Fatalf("replicate: %v", err)
	}

	// Every sidecar really is absent — the premise of the test.
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("manifests/_replicas/db1.full.%03d.manifest.json", i)
		if statExists(t, dstRaw, key) {
			t.Fatalf("%s unexpectedly present; the fault rule did not bite", key)
		}
	}

	if res.ManifestReplicasFailed != n {
		t.Errorf("ManifestReplicasFailed = %d, want %d — the counter must be unbounded, or "+
			"failures past the %d-entry detail cap are reported nowhere at all",
			res.ManifestReplicasFailed, n, len(res.Failures))
	}
	// The primaries landed, so the headline verdict must stay clean:
	// best-effort failures are not primary-replication failures.
	if res.ManifestsFailed != 0 {
		t.Errorf("ManifestsFailed = %d, want 0 — a replica sidecar failure must not be "+
			"reported as a primary-manifest failure", res.ManifestsFailed)
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("manifests/db1/backups/db1.full.%03d/manifest.json", i)
		if !statExists(t, dstRaw, key) {
			t.Fatalf("primary %s missing — the fault rule hit more than the sidecars", key)
		}
	}
}

// Under the cap the counter and the detail list must agree, so the
// two numbers can be trusted against each other.
func TestReplicate_ReplicaFailureCounterAgreesWithDetailUnderTheCap(t *testing.T) {
	src, dstRaw := twoRepos(t)
	const n = 3
	seedBackupsWithSidecars(t, src, n)

	res, err := repo.Replicate(context.Background(), src,
		failReplicaPuts(dstRaw, errors.New("dst rejected")), repo.ReplicateOptions{})
	if err != nil {
		t.Fatalf("replicate: %v", err)
	}
	if res.ManifestReplicasFailed != n || len(res.Failures) != n {
		t.Errorf("ManifestReplicasFailed=%d len(Failures)=%d, want %d and %d",
			res.ManifestReplicasFailed, len(res.Failures), n, n)
	}
}

// A clean run must leave the counter at zero, or the new failure line
// would fire on healthy replications.
func TestReplicate_CleanRunLeavesReplicaCounterZero(t *testing.T) {
	src, dst := twoRepos(t)
	seedBackupsWithSidecars(t, src, 3)

	res, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatalf("replicate: %v", err)
	}
	if res.ManifestReplicasFailed != 0 {
		t.Errorf("ManifestReplicasFailed = %d on a clean run", res.ManifestReplicasFailed)
	}
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("manifests/_replicas/db1.full.%03d.manifest.json", i)
		if !statExists(t, dst, key) {
			t.Errorf("%s missing after a clean replication", key)
		}
	}
}

// ManifestsCopied counts manifest OBJECTS, not backups: a backup with
// a sidecar contributes two. This is long-published JSON under the
// 24-month schema commitment, so it is pinned rather than changed —
// a future edit to the meaning should have to delete this test on
// purpose.
func TestReplicate_ManifestsCopiedCountsSidecarsToo(t *testing.T) {
	src, dst := twoRepos(t)
	const n = 3
	seedBackupsWithSidecars(t, src, n)

	res, err := repo.Replicate(context.Background(), src, dst, repo.ReplicateOptions{})
	if err != nil {
		t.Fatalf("replicate: %v", err)
	}
	if res.ManifestsConsidered != n {
		t.Errorf("ManifestsConsidered = %d, want %d", res.ManifestsConsidered, n)
	}
	if res.ManifestsCopied != 2*n {
		t.Errorf("ManifestsCopied = %d, want %d (primary + sidecar per backup)",
			res.ManifestsCopied, 2*n)
	}
}

// The other half of the contract: a PRIMARY manifest failure is not
// best-effort and must land in ManifestsFailed. Without this the
// counter split above could be satisfied by never counting anything.
func TestReplicate_PrimaryManifestFailureCountsAsAFailure(t *testing.T) {
	src, dstRaw := twoRepos(t)
	seedBackupsWithSidecars(t, src, 3)

	m := faultinject.New(dstRaw)
	m.Activate([]faultinject.Rule{{
		Name: "primary-manifest-put", Ops: faultinject.OpPut,
		KeyPrefix: "manifests/db1/", Err: errors.New("dst rejected"),
	}}, faultinject.ActivateOptions{})

	res, err := repo.Replicate(context.Background(), src, m, repo.ReplicateOptions{})
	if err != nil {
		t.Fatalf("replicate: %v", err)
	}
	if res.ManifestsFailed != 3 {
		t.Errorf("ManifestsFailed = %d, want 3 — a primary manifest that did not reach the "+
			"destination is a replication failure", res.ManifestsFailed)
	}
	if res.ManifestReplicasFailed != 0 {
		t.Errorf("ManifestReplicasFailed = %d, want 0 — only the primaries were failed",
			res.ManifestReplicasFailed)
	}
}
