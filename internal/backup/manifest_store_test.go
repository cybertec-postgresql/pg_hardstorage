package backup_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

func newStore(t *testing.T) (*backup.ManifestStore, storage.StoragePlugin, *backup.Signer, *backup.Verifier) {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := backup.LoadSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := backup.LoadVerifier(pub)
	if err != nil {
		t.Fatal(err)
	}
	return backup.NewManifestStore(sp), sp, signer, verifier
}

func TestCommit_RoundTrip(t *testing.T) {
	store, _, signer, verifier := newStore(t)
	m := sampleManifest()

	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	got, err := store.Read(context.Background(), m.Deployment, m.BackupID, verifier)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.BackupID != m.BackupID {
		t.Errorf("BackupID round-trip: %q != %q", got.BackupID, m.BackupID)
	}
	if len(got.Files) != len(m.Files) {
		t.Errorf("Files: got %d want %d", len(got.Files), len(m.Files))
	}
}

func TestCommit_RaceProducesOneCommit(t *testing.T) {
	store, _, signer, _ := newStore(t)

	var wins atomic.Int32
	var alreadyExists atomic.Int32
	var wg sync.WaitGroup
	const N = 8
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			m := sampleManifest()
			err := store.Commit(context.Background(), m, signer, backup.CommitOptions{})
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, backup.ErrAlreadyCommitted):
				alreadyExists.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Errorf("expected exactly one Commit winner; got %d", got)
	}
	if got := alreadyExists.Load(); got != N-1 {
		t.Errorf("expected %d ErrAlreadyCommitted; got %d", N-1, got)
	}
}

func TestCommit_ReplicaWritten(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	m := sampleManifest()
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	if _, err := sp.Stat(context.Background(), backup.PrimaryPath(m.Deployment, m.BackupID)); err != nil {
		t.Errorf("primary missing: %v", err)
	}
	if _, err := sp.Stat(context.Background(), backup.ReplicaPath(m.BackupID)); err != nil {
		t.Errorf("replica missing: %v", err)
	}
}

// replicaFailingSP wraps a StoragePlugin and fails any Put whose key
// matches the replica path prefix, leaving primary writes alone. Used
// to assert OnReplicaError fires while the primary commit still
// succeeds.
type replicaFailingSP struct {
	storage.StoragePlugin
	fail error
}

func (s *replicaFailingSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if strings.HasPrefix(key, "manifests/_replicas/") {
		return storage.PutResult{}, s.fail
	}
	return s.StoragePlugin.Put(ctx, key, r, opts)
}

func TestCommit_OnReplicaError_FiresWhenReplicaFails(t *testing.T) {
	root := t.TempDir()
	inner := &fs.Plugin{}
	if err := inner.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	wrapped := &replicaFailingSP{StoragePlugin: inner, fail: errors.New("replica disk full")}

	priv, _, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := backup.LoadSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	store := backup.NewManifestStore(wrapped)
	m := sampleManifest()

	var captured error
	cbCalls := atomic.Int32{}
	opts := backup.CommitOptions{
		OnReplicaError: func(rerr error) {
			cbCalls.Add(1)
			captured = rerr
		},
	}
	// The commit must succeed despite the replica write failing — the
	// primary is authoritative.
	if err := store.Commit(context.Background(), m, signer, opts); err != nil {
		t.Fatalf("commit returned error despite primary success: %v", err)
	}

	// Primary present, replica absent.
	if _, err := inner.Stat(context.Background(), backup.PrimaryPath(m.Deployment, m.BackupID)); err != nil {
		t.Errorf("primary missing after commit: %v", err)
	}
	if _, err := inner.Stat(context.Background(), backup.ReplicaPath(m.BackupID)); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("replica should be absent (write failed); got err %v", err)
	}

	// Callback fired exactly once with the wrapped error.
	if got := cbCalls.Load(); got != 1 {
		t.Errorf("OnReplicaError calls = %d; want 1", got)
	}
	if captured == nil || !strings.Contains(captured.Error(), "replica disk full") {
		t.Errorf("OnReplicaError received %v; expected to wrap replica fail", captured)
	}
	if captured != nil && !strings.Contains(captured.Error(), backup.ReplicaPath(m.BackupID)) {
		t.Errorf("OnReplicaError should mention the replica key; got %v", captured)
	}
}

func TestCommit_OnReplicaError_NotCalledOnSuccess(t *testing.T) {
	store, _, signer, _ := newStore(t)
	cbCalls := atomic.Int32{}
	if err := store.Commit(context.Background(), sampleManifest(), signer,
		backup.CommitOptions{OnReplicaError: func(error) { cbCalls.Add(1) }}); err != nil {
		t.Fatal(err)
	}
	if got := cbCalls.Load(); got != 0 {
		t.Errorf("OnReplicaError must not fire on replica success; got %d calls", got)
	}
}

func TestCommit_SkipReplica(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	m := sampleManifest()
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{SkipReplica: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Stat(context.Background(), backup.ReplicaPath(m.BackupID)); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("replica should be absent when SkipReplica=true; got err %v", err)
	}
}

func TestRead_FallsBackToReplicaWhenPrimaryDeleted(t *testing.T) {
	store, sp, signer, verifier := newStore(t)
	m := sampleManifest()
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	// Delete the primary; the replica still has the bytes.
	if err := sp.Delete(context.Background(), backup.PrimaryPath(m.Deployment, m.BackupID)); err != nil {
		t.Fatal(err)
	}
	got, err := store.Read(context.Background(), m.Deployment, m.BackupID, verifier)
	if err != nil {
		t.Fatalf("Read should fall through to replica: %v", err)
	}
	if got.BackupID != m.BackupID {
		t.Errorf("replica returned a different manifest: %s vs %s", got.BackupID, m.BackupID)
	}
}

func TestRead_BothMissing(t *testing.T) {
	store, _, _, verifier := newStore(t)
	_, err := store.Read(context.Background(), "nodep", "nope", verifier)
	if err == nil {
		t.Fatal("read of missing manifest should fail")
	}
}

func TestRead_RejectsForeignSigner(t *testing.T) {
	store, _, signer, _ := newStore(t)
	_, foreignPub, _ := backup.GenerateKeypair(rand.Reader)
	foreignVerifier, _ := backup.LoadVerifier(foreignPub)

	m := sampleManifest()
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := store.Read(context.Background(), m.Deployment, m.BackupID, foreignVerifier)
	if err == nil {
		t.Fatal("Read with a foreign verifier must fail")
	}
}

func TestList_ReturnsAllCommittedManifests(t *testing.T) {
	store, _, signer, verifier := newStore(t)

	want := map[string]bool{}
	for _, id := range []string{"db1.full.20260428T1200Z", "db1.full.20260428T1300Z", "db1.full.20260428T1400Z"} {
		m := sampleManifest()
		m.BackupID = id
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatal(err)
		}
		want[id] = true
	}

	got := map[string]bool{}
	for m, err := range store.List(context.Background(), "db1", verifier) {
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		got[m.BackupID] = true
	}
	if len(got) != len(want) {
		t.Errorf("got %v want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("missing %q in list", id)
		}
	}
}

func TestDeployments_EnumeratesUniqueNames(t *testing.T) {
	store, _, signer, verifier := newStore(t)
	for _, dep := range []string{"db1", "db2", "analytics"} {
		for _, id := range []string{"first", "second"} {
			m := sampleManifest()
			m.Deployment = dep
			m.BackupID = dep + "." + id
			if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := store.Deployments(context.Background())
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	want := []string{"analytics", "db1", "db2"} // sorted
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %q, want %q", i, got[i], w)
		}
	}
	_ = verifier // unused but kept for parity with other helpers
}

func TestDeployments_EmptyRepo(t *testing.T) {
	store, _, _, _ := newStore(t)
	got, err := store.Deployments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty repo should yield 0 deployments; got %v", got)
	}
}

func TestDeployments_ExcludesReplicasPseudoDir(t *testing.T) {
	store, _, signer, _ := newStore(t)
	if err := store.Commit(context.Background(), sampleManifest(), signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Deployments(context.Background())
	for _, name := range got {
		if name == "_replicas" {
			t.Error("Deployments must not include the _replicas pseudo-dir")
		}
	}
}

func TestSoftDelete_HidesFromReadAndList(t *testing.T) {
	store, sp, signer, verifier := newStore(t)

	// Commit two backups; soft-delete one.
	m1 := sampleManifest()
	m1.BackupID = "db1.full.20260428T1200Z"
	m2 := sampleManifest()
	m2.BackupID = "db1.full.20260428T1300Z"
	for _, m := range []*backup.Manifest{m1, m2} {
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.SoftDelete(context.Background(), "db1", m1.BackupID, "gfs", "older than retention"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	// Read of tombstoned backup must return ErrTombstoned.
	if _, err := store.Read(context.Background(), "db1", m1.BackupID, verifier); !errors.Is(err, backup.ErrTombstoned) {
		t.Errorf("Read of tombstoned manifest should return ErrTombstoned; got %v", err)
	}

	// Read of the live one still works.
	if _, err := store.Read(context.Background(), "db1", m2.BackupID, verifier); err != nil {
		t.Errorf("Read of live manifest: %v", err)
	}

	// List should yield exactly the live manifest.
	got := []string{}
	for m, err := range store.List(context.Background(), "db1", verifier) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, m.BackupID)
	}
	if len(got) != 1 || got[0] != m2.BackupID {
		t.Errorf("List = %v, want exactly %v", got, []string{m2.BackupID})
	}

	// Tombstone marker exists at the canonical path.
	if _, err := sp.Stat(context.Background(), backup.TombstonePath("db1", m1.BackupID)); err != nil {
		t.Errorf("tombstone marker missing: %v", err)
	}
}

// TestList_TombstonedDoesNotYieldNilManifest is a paranoia test
// against a class of bug a reviewer flagged: "if the tombstone-skip
// uses `continue`, does the loop fall through to yield(nil, nil)
// after readVerified is skipped?" The answer is "no, because Go's
// `continue` jumps to the next loop iteration, not past it" — but
// asserting it explicitly here documents the contract for the next
// reader of the code.
func TestList_YieldsManifestsInDeterministicOrder(t *testing.T) {
	// Even if the backend's List returns keys in some other order,
	// ManifestStore.List MUST yield in lexicographic key order so
	// the v1-schema contract is reproducible across backends. We
	// commit IDs in random order and assert the output is sorted.
	store, _, signer, verifier := newStore(t)

	// IDs intentionally NOT in alphabetical order at commit time.
	commitOrder := []string{
		"db1.full.20260428T1500Z",
		"db1.full.20260428T0900Z",
		"db1.full.20260428T1200Z",
		"db1.full.20260428T0300Z",
		"db1.full.20260428T2100Z",
	}
	for _, id := range commitOrder {
		m := sampleManifest()
		m.BackupID = id
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	for m, err := range store.List(context.Background(), "db1", verifier) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, m.BackupID)
	}
	want := []string{
		"db1.full.20260428T0300Z",
		"db1.full.20260428T0900Z",
		"db1.full.20260428T1200Z",
		"db1.full.20260428T1500Z",
		"db1.full.20260428T2100Z",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q (full got: %v)", i, got[i], want[i], got)
		}
	}
}

func TestRead_BothMissing_ReturnsErrNotFound(t *testing.T) {
	// When both primary and replica are absent, Read should return
	// a clean storage.ErrNotFound — not a noisy "primary failed
	// and replica failed" message that just says the same thing
	// twice. Callers errors.Is the returned value.
	store, _, _, verifier := newStore(t)
	_, err := store.Read(context.Background(), "ghost", "nope", verifier)
	if err == nil {
		t.Fatal("expected error reading missing manifest")
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected errors.Is(err, storage.ErrNotFound) = true; got err = %v", err)
	}
}

func TestRead_MixedFailure_PreservesBothErrors(t *testing.T) {
	// If the primary AND replica fail for DIFFERENT reasons (e.g.
	// primary is corrupt, replica is genuinely missing), the
	// returned error should keep both visible — that's the
	// forensic value the noisy form was designed for.
	store, sp, signer, verifier := newStore(t)
	m := sampleManifest()
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	// Corrupt the primary by overwriting it with garbage; leave
	// the replica intact so the read CAN fall through to it.
	if _, err := sp.Put(context.Background(), backup.PrimaryPath(m.Deployment, m.BackupID),
		bytes.NewReader([]byte("not a manifest")), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// Then delete the replica too so we hit a mixed-failure case
	// (primary corrupt, replica not-found).
	if err := sp.Delete(context.Background(), backup.ReplicaPath(m.BackupID)); err != nil {
		t.Fatal(err)
	}

	_, err := store.Read(context.Background(), m.Deployment, m.BackupID, verifier)
	if err == nil {
		t.Fatal("expected error")
	}
	// MUST NOT collapse to plain ErrNotFound — that would hide the
	// fact that the primary is corrupt.
	if errors.Is(err, storage.ErrNotFound) {
		t.Errorf("mixed-failure case incorrectly collapsed to ErrNotFound; err = %v", err)
	}
	// Forensic detail still present in the message.
	if !strings.Contains(err.Error(), "primary failed") {
		t.Errorf("error should mention primary failed; got %v", err)
	}
}

func TestList_TombstonedDoesNotYieldNilManifest(t *testing.T) {
	store, _, signer, verifier := newStore(t)

	// Mix of live and tombstoned manifests. We tombstone EVERY
	// committed manifest, then iterate; if `continue` were buggy
	// we'd see len(N) yields with nil m and nil err.
	for _, id := range []string{"a", "b", "c"} {
		m := sampleManifest()
		m.BackupID = "db1.full." + id
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := store.SoftDelete(context.Background(), "db1", m.BackupID, "test", ""); err != nil {
			t.Fatal(err)
		}
	}

	yields := 0
	nilManifestNilErr := 0
	for m, err := range store.List(context.Background(), "db1", verifier) {
		yields++
		if m == nil && err == nil {
			nilManifestNilErr++
		}
	}
	if yields != 0 {
		t.Errorf("List should yield 0 entries when every manifest is tombstoned; got %d", yields)
	}
	if nilManifestNilErr > 0 {
		t.Errorf("List should never yield (nil, nil); got %d such yields", nilManifestNilErr)
	}
}

func TestSoftDelete_Idempotent(t *testing.T) {
	store, _, signer, _ := newStore(t)
	m := sampleManifest()
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := store.SoftDelete(context.Background(), m.Deployment, m.BackupID, "gfs", "test"); err != nil {
			t.Errorf("SoftDelete iter %d: %v", i, err)
		}
	}
}

func TestIsTombstoned(t *testing.T) {
	store, _, signer, _ := newStore(t)
	m := sampleManifest()
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	dead, err := store.IsTombstoned(context.Background(), m.Deployment, m.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	if dead {
		t.Error("fresh manifest should not be tombstoned")
	}
	_ = store.SoftDelete(context.Background(), m.Deployment, m.BackupID, "manual", "")
	dead, err = store.IsTombstoned(context.Background(), m.Deployment, m.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	if !dead {
		t.Error("after SoftDelete, IsTombstoned should be true")
	}
}

func TestNewManifestStore_NilPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil StoragePlugin")
		}
	}()
	backup.NewManifestStore(nil)
}

func TestCommit_RequiresDeploymentAndID(t *testing.T) {
	store, _, signer, _ := newStore(t)
	for _, m := range []*backup.Manifest{
		{Deployment: "", BackupID: "x"},
		{Deployment: "x", BackupID: ""},
	} {
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err == nil {
			t.Errorf("commit with empty deployment/id must fail; got nil for %+v", m)
		}
	}
}

func TestCommit_NeedsSignerWhenUnsigned(t *testing.T) {
	store, _, _, _ := newStore(t)
	m := sampleManifest()
	if err := store.Commit(context.Background(), m, nil, backup.CommitOptions{}); err == nil {
		t.Error("commit without signer on unsigned manifest must fail")
	}
}

func TestCommit_AcceptsPreSignedManifest(t *testing.T) {
	store, _, signer, verifier := newStore(t)
	m := sampleManifest()
	if err := m.Sign(signer); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), m, nil, backup.CommitOptions{}); err != nil {
		t.Fatalf("pre-signed commit should succeed: %v", err)
	}
	if _, err := store.Read(context.Background(), m.Deployment, m.BackupID, verifier); err != nil {
		t.Errorf("read after pre-signed commit: %v", err)
	}
}
