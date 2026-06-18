package backup_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// rotateWorld is a small fixture: an init'd repo, a signing
// keypair, and helpers to plant encrypted manifests.
type rotateWorld struct {
	sp       storage.StoragePlugin
	store    *backup.ManifestStore
	signer   *backup.Signer
	verifier *backup.Verifier
}

func setupRotateWorld(t *testing.T) *rotateWorld {
	t.Helper()
	root := t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: "file://" + root}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	return &rotateWorld{
		sp:       sp,
		store:    backup.NewManifestStore(sp),
		signer:   signer,
		verifier: verifier,
	}
}

// commitEncrypted plants an encrypted backup manifest wrapped under
// kek with the given kekRef. The chunk is written via an
// encryption-aware CAS so the on-disk envelope is realistic.
func (w *rotateWorld) commitEncrypted(t *testing.T, deployment, backupID string, kek [encryption.KeyLen]byte, kekRef string, idx int) {
	t.Helper()
	var dek [encryption.KeyLen]byte
	if _, err := rand.Read(dek[:]); err != nil {
		t.Fatal(err)
	}
	wrapped, err := encryption.Wrap(kek, dek)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := aesgcm.New(dek[:])
	if err != nil {
		t.Fatal(err)
	}
	cas := casdefault.NewEncrypted(w.sp, enc)
	chunkBody := []byte("encrypted-payload-" + backupID)
	info, err := cas.PutChunk(context.Background(), chunkBody)
	if err != nil {
		t.Fatal(err)
	}
	stoppedAt := time.Date(2026, 4, 30, 12, idx, 0, 0, time.UTC)
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         backupID,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        stoppedAt.Add(-30 * time.Second),
		StoppedAt:        stoppedAt,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces: []backup.Tablespace{
			{OID: 1663, Location: "pg_default"},
		},
		Encryption: &backup.EncryptionInfo{
			Scheme:          "aes-256-gcm",
			KEKRef:          kekRef,
			WrappedDEK:      base64.StdEncoding.EncodeToString(wrapped),
			EnvelopeVersion: 1,
		},
		Files: []backup.FileEntry{{
			Path: "data/" + backupID,
			Size: int64(len(chunkBody)),
			Mode: 0o600,
			Chunks: []backup.ChunkRef{{
				Hash:   info.Hash,
				Offset: 0,
				Len:    int64(len(chunkBody)),
			}},
		}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
}

// commitUnencrypted plants a non-encrypted manifest.
func (w *rotateWorld) commitUnencrypted(t *testing.T, deployment, backupID string, idx int) {
	t.Helper()
	cas := casdefault.New(w.sp)
	chunkBody := []byte("plain-" + backupID)
	info, err := cas.PutChunk(context.Background(), chunkBody)
	if err != nil {
		t.Fatal(err)
	}
	stoppedAt := time.Date(2026, 4, 30, 12, idx, 0, 0, time.UTC)
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         backupID,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        stoppedAt.Add(-30 * time.Second),
		StoppedAt:        stoppedAt,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces: []backup.Tablespace{
			{OID: 1663, Location: "pg_default"},
		},
		Files: []backup.FileEntry{{
			Path: "data/" + backupID,
			Size: int64(len(chunkBody)),
			Mode: 0o600,
			Chunks: []backup.ChunkRef{{
				Hash:   info.Hash,
				Offset: 0,
				Len:    int64(len(chunkBody)),
			}},
		}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
}

func mkKEK(t *testing.T) [encryption.KeyLen]byte {
	t.Helper()
	var k [encryption.KeyLen]byte
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatal(err)
	}
	return k
}

// TestRotateKEK_RoundTrip: encrypted backup wrapped under OLD KEK
// → rotate → manifest now references NEW KEK ref + new wrapped
// DEK; the CAS's chunks still decrypt cleanly because the underlying
// DEK is unchanged.
func TestRotateKEK_RoundTrip(t *testing.T) {
	w := setupRotateWorld(t)
	oldKEK := mkKEK(t)
	newKEK := mkKEK(t)

	w.commitEncrypted(t, "db1", "db1.full.aaa", oldKEK, "test:old", 1)

	res, err := backup.RotateKEK(context.Background(), w.sp, backup.RotateKEKOptions{
		OldKEKRef: "test:old",
		OldKEK:    oldKEK,
		NewKEKRef: "test:new",
		NewKEK:    newKEK,
		Signer:    w.signer,
		Verifier:  w.verifier,
	})
	if err != nil {
		t.Fatalf("RotateKEK: %v", err)
	}
	if res.Rotated != 1 {
		t.Errorf("Rotated=%d, want 1", res.Rotated)
	}
	if res.Failed != 0 {
		t.Errorf("unexpected failures: %+v", res.Failures)
	}

	// Reload the manifest and confirm the encryption block flipped.
	m, err := w.store.Read(context.Background(), "db1", "db1.full.aaa", w.verifier)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if m.Encryption.KEKRef != "test:new" {
		t.Errorf("KEKRef = %q, want test:new", m.Encryption.KEKRef)
	}
	// Wrapped DEK should be DIFFERENT bytes than the original
	// (different KEK = different wrap output).
	wrappedBytes, err := base64.StdEncoding.DecodeString(m.Encryption.WrappedDEK)
	if err != nil {
		t.Fatal(err)
	}
	// The DEK can be recovered with the NEW KEK.
	dek, err := encryption.Unwrap(newKEK, wrappedBytes)
	if err != nil {
		t.Errorf("unwrap with new KEK failed: %v", err)
	}
	if dek == ([encryption.KeyLen]byte{}) {
		t.Error("unwrapped DEK is zero — corruption")
	}
	// And the old KEK no longer works.
	if _, err := encryption.Unwrap(oldKEK, wrappedBytes); err == nil {
		t.Error("old KEK should NOT unwrap the rotated manifest")
	}
}

// TestRotateKEK_UnencryptedSkipped: unencrypted manifests are
// silently skipped (no error, counted in SkippedUnencrypted).
func TestRotateKEK_UnencryptedSkipped(t *testing.T) {
	w := setupRotateWorld(t)
	oldKEK := mkKEK(t)
	newKEK := mkKEK(t)
	w.commitUnencrypted(t, "db1", "db1.full.plain", 1)

	res, err := backup.RotateKEK(context.Background(), w.sp, backup.RotateKEKOptions{
		OldKEKRef: "test:old",
		OldKEK:    oldKEK,
		NewKEKRef: "test:new",
		NewKEK:    newKEK,
		Signer:    w.signer,
		Verifier:  w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SkippedUnencrypted != 1 {
		t.Errorf("SkippedUnencrypted=%d, want 1", res.SkippedUnencrypted)
	}
	if res.Rotated != 0 {
		t.Errorf("plain manifest should not be rotated; Rotated=%d", res.Rotated)
	}
}

// TestRotateKEK_DifferentKEKRefSkipped: a manifest wrapped with
// some-other-kek-ref is skipped, not failed. This is critical for
// multi-tenant repos where each tenant rotates independently.
func TestRotateKEK_DifferentKEKRefSkipped(t *testing.T) {
	w := setupRotateWorld(t)
	tenantAKEK := mkKEK(t)
	tenantBKEK := mkKEK(t)
	newAKEK := mkKEK(t)
	w.commitEncrypted(t, "db1", "db1.full.tenantA", tenantAKEK, "tenant-a:v1", 1)
	w.commitEncrypted(t, "db1", "db1.full.tenantB", tenantBKEK, "tenant-b:v1", 2)

	res, err := backup.RotateKEK(context.Background(), w.sp, backup.RotateKEKOptions{
		OldKEKRef: "tenant-a:v1",
		OldKEK:    tenantAKEK,
		NewKEKRef: "tenant-a:v2",
		NewKEK:    newAKEK,
		Signer:    w.signer,
		Verifier:  w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rotated != 1 {
		t.Errorf("tenant-a should rotate; Rotated=%d", res.Rotated)
	}
	if res.SkippedDifferentKEK != 1 {
		t.Errorf("tenant-b should be skipped; got %d", res.SkippedDifferentKEK)
	}
}

// TestRotateKEK_DryRunNoMutation: with DryRun=true, the rotation
// reports the plan but the manifest is unchanged on disk.
func TestRotateKEK_DryRunNoMutation(t *testing.T) {
	w := setupRotateWorld(t)
	oldKEK := mkKEK(t)
	newKEK := mkKEK(t)
	w.commitEncrypted(t, "db1", "db1.full.dry", oldKEK, "test:old", 1)

	beforeMani, err := w.store.Read(context.Background(), "db1", "db1.full.dry", w.verifier)
	if err != nil {
		t.Fatal(err)
	}
	beforeWrapped := beforeMani.Encryption.WrappedDEK

	res, err := backup.RotateKEK(context.Background(), w.sp, backup.RotateKEKOptions{
		OldKEKRef: "test:old",
		OldKEK:    oldKEK,
		NewKEKRef: "test:new",
		NewKEK:    newKEK,
		Signer:    w.signer,
		Verifier:  w.verifier,
		DryRun:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rotated != 1 {
		t.Errorf("dry-run should still report 1 candidate; got %d", res.Rotated)
	}

	afterMani, err := w.store.Read(context.Background(), "db1", "db1.full.dry", w.verifier)
	if err != nil {
		t.Fatal(err)
	}
	if afterMani.Encryption.KEKRef != "test:old" {
		t.Errorf("dry-run mutated KEKRef: %q", afterMani.Encryption.KEKRef)
	}
	if afterMani.Encryption.WrappedDEK != beforeWrapped {
		t.Errorf("dry-run mutated wrapped_dek")
	}
}

// TestRotateKEK_AlreadyRotatedIsIdempotent: re-running a rotation
// on a manifest already wrapped with NewKEKRef is a clean no-op.
// This makes a partially-completed rotation safely resumable.
func TestRotateKEK_AlreadyRotatedIsIdempotent(t *testing.T) {
	w := setupRotateWorld(t)
	oldKEK := mkKEK(t)
	newKEK := mkKEK(t)
	w.commitEncrypted(t, "db1", "db1.full.idem", oldKEK, "test:old", 1)

	// First rotation.
	if _, err := backup.RotateKEK(context.Background(), w.sp, backup.RotateKEKOptions{
		OldKEKRef: "test:old",
		OldKEK:    oldKEK,
		NewKEKRef: "test:new",
		NewKEK:    newKEK,
		Signer:    w.signer,
		Verifier:  w.verifier,
	}); err != nil {
		t.Fatal(err)
	}
	// Second rotation with the same args — should classify as
	// AlreadyRotated.
	res, err := backup.RotateKEK(context.Background(), w.sp, backup.RotateKEKOptions{
		OldKEKRef: "test:old",
		OldKEK:    oldKEK,
		NewKEKRef: "test:new",
		NewKEK:    newKEK,
		Signer:    w.signer,
		Verifier:  w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.AlreadyRotated != 1 {
		t.Errorf("AlreadyRotated=%d, want 1", res.AlreadyRotated)
	}
	if res.Rotated != 0 {
		t.Errorf("Rotated=%d, want 0 on second run", res.Rotated)
	}
}

// TestRotateKEK_WrongOldKEKFailsCleanly: supplying a KEK that
// doesn't unwrap the manifest's DEK records a failure but doesn't
// abort the run.
func TestRotateKEK_WrongOldKEKFailsCleanly(t *testing.T) {
	w := setupRotateWorld(t)
	rightKEK := mkKEK(t)
	wrongKEK := mkKEK(t)
	newKEK := mkKEK(t)
	w.commitEncrypted(t, "db1", "db1.full.wrong-kek", rightKEK, "test:old", 1)

	res, err := backup.RotateKEK(context.Background(), w.sp, backup.RotateKEKOptions{
		OldKEKRef: "test:old",
		OldKEK:    wrongKEK, // doesn't match what wrapped this manifest
		NewKEKRef: "test:new",
		NewKEK:    newKEK,
		Signer:    w.signer,
		Verifier:  w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 {
		t.Errorf("Failed=%d, want 1", res.Failed)
	}
	if res.Rotated != 0 {
		t.Errorf("Rotated=%d, want 0 (wrong KEK should not rotate)", res.Rotated)
	}
	if len(res.Failures) == 0 {
		t.Error("expected per-key Failures detail")
	}
}

// TestRotateKEK_PostRotationManifestStillReadable: after rotation,
// the manifest can be Read through the normal store + verifier
// path. Catches "we re-signed it correctly" regressions.
func TestRotateKEK_PostRotationManifestStillReadable(t *testing.T) {
	w := setupRotateWorld(t)
	oldKEK := mkKEK(t)
	newKEK := mkKEK(t)
	w.commitEncrypted(t, "db1", "db1.full.read-after", oldKEK, "test:old", 1)

	if _, err := backup.RotateKEK(context.Background(), w.sp, backup.RotateKEKOptions{
		OldKEKRef: "test:old",
		OldKEK:    oldKEK,
		NewKEKRef: "test:new",
		NewKEK:    newKEK,
		Signer:    w.signer,
		Verifier:  w.verifier,
	}); err != nil {
		t.Fatal(err)
	}

	m, err := w.store.Read(context.Background(), "db1", "db1.full.read-after", w.verifier)
	if err != nil {
		t.Fatalf("post-rotation Read: %v (suggests signing or canonicalisation broke)", err)
	}
	if m.BackupID != "db1.full.read-after" {
		t.Errorf("BackupID round-trip wrong: %q", m.BackupID)
	}
	if len(m.Files) != 1 {
		t.Errorf("Files round-trip wrong: %d", len(m.Files))
	}
}

// TestRotateKEK_MultipleManifests: a repo with several manifests
// (some target, some other-tenant, some plain) rotates only the
// targeted ones.
func TestRotateKEK_MultipleManifests(t *testing.T) {
	w := setupRotateWorld(t)
	targetKEK := mkKEK(t)
	otherKEK := mkKEK(t)
	newKEK := mkKEK(t)

	w.commitEncrypted(t, "db1", "db1.full.A", targetKEK, "target:v1", 1)
	w.commitEncrypted(t, "db1", "db1.full.B", targetKEK, "target:v1", 2)
	w.commitEncrypted(t, "db1", "db1.full.C", otherKEK, "other:v1", 3)
	w.commitUnencrypted(t, "db1", "db1.full.D", 4)
	w.commitEncrypted(t, "db2", "db2.full.E", targetKEK, "target:v1", 5)

	res, err := backup.RotateKEK(context.Background(), w.sp, backup.RotateKEKOptions{
		OldKEKRef: "target:v1",
		OldKEK:    targetKEK,
		NewKEKRef: "target:v2",
		NewKEK:    newKEK,
		Signer:    w.signer,
		Verifier:  w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 3 target backups (A, B, E across two deployments).
	if res.Rotated != 3 {
		t.Errorf("Rotated=%d, want 3", res.Rotated)
	}
	if res.SkippedDifferentKEK != 1 {
		t.Errorf("SkippedDifferentKEK=%d, want 1 (other-tenant)", res.SkippedDifferentKEK)
	}
	if res.SkippedUnencrypted != 1 {
		t.Errorf("SkippedUnencrypted=%d, want 1 (plain D)", res.SkippedUnencrypted)
	}
	if res.Considered != 5 {
		t.Errorf("Considered=%d, want 5", res.Considered)
	}
}

// TestRotateKEK_OnProgressCallback fires per manifest with the
// classification outcome.
func TestRotateKEK_OnProgressCallback(t *testing.T) {
	w := setupRotateWorld(t)
	oldKEK := mkKEK(t)
	newKEK := mkKEK(t)
	w.commitEncrypted(t, "db1", "db1.full.progress", oldKEK, "test:old", 1)

	var outcomes []string
	if _, err := backup.RotateKEK(context.Background(), w.sp, backup.RotateKEKOptions{
		OldKEKRef: "test:old",
		OldKEK:    oldKEK,
		NewKEKRef: "test:new",
		NewKEK:    newKEK,
		Signer:    w.signer,
		Verifier:  w.verifier,
		OnProgress: func(p backup.RotateKEKProgress) {
			outcomes = append(outcomes, p.Outcome)
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 || outcomes[0] != "rotated" {
		t.Errorf("outcomes=%v, want [rotated]", outcomes)
	}
}

// TestRotateKEK_ValidationErrors: required fields surface as
// errors (nil signer, missing refs, same-old-and-new).
func TestRotateKEK_ValidationErrors(t *testing.T) {
	w := setupRotateWorld(t)
	k := mkKEK(t)
	base := backup.RotateKEKOptions{
		OldKEKRef: "old", OldKEK: k,
		NewKEKRef: "new", NewKEK: k,
		Signer: w.signer, Verifier: w.verifier,
	}
	cases := []struct {
		name string
		mut  func(o *backup.RotateKEKOptions)
	}{
		{"empty old ref", func(o *backup.RotateKEKOptions) { o.OldKEKRef = "" }},
		{"empty new ref", func(o *backup.RotateKEKOptions) { o.NewKEKRef = "" }},
		{"same refs", func(o *backup.RotateKEKOptions) { o.NewKEKRef = "old" }},
		{"nil signer", func(o *backup.RotateKEKOptions) { o.Signer = nil }},
		{"nil verifier", func(o *backup.RotateKEKOptions) { o.Verifier = nil }},
	}
	for _, c := range cases {
		opts := base
		c.mut(&opts)
		if _, err := backup.RotateKEK(context.Background(), w.sp, opts); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}
}

// TestRotateKEK_NilStoragePlugin obvious validation guard.
func TestRotateKEK_NilStoragePlugin(t *testing.T) {
	if _, err := backup.RotateKEK(context.Background(), nil, backup.RotateKEKOptions{
		OldKEKRef: "old", NewKEKRef: "new",
	}); err == nil {
		t.Error("expected error for nil StoragePlugin")
	}
}

func rotGetKey(t *testing.T, sp storage.StoragePlugin, key string) []byte {
	t.Helper()
	rc, err := sp.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return b
}

func rotPutKey(t *testing.T, sp storage.StoragePlugin, key string, body []byte) {
	t.Helper()
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func rotReplicaKEKRef(t *testing.T, sp storage.StoragePlugin, key string) string {
	t.Helper()
	m, err := backup.ParseAttestationless(rotGetKey(t, sp, key))
	if err != nil {
		t.Fatalf("parse %s: %v", key, err)
	}
	if m.Encryption == nil {
		return ""
	}
	return m.Encryption.KEKRef
}

// TestRotateKEK_ResumeHealsStrandedReplica pins the data-loss fix: when a
// prior rotation rotated the PRIMARY but left the REPLICA on the old KEK
// (a replica-write failure, or a crash between the two writes), a re-run
// MUST heal the replica. Before the fix the re-run saw the primary
// already rotated and skipped the manifest entirely, stranding the
// replica — which becomes undecryptable the moment the old KEK is retired.
func TestRotateKEK_ResumeHealsStrandedReplica(t *testing.T) {
	w := setupRotateWorld(t)
	oldKEK := mkKEK(t)
	newKEK := mkKEK(t)
	w.commitEncrypted(t, "db1", "db1.full.heal", oldKEK, "test:old", 1)

	replicaKey := backup.ReplicaPath("db1.full.heal")
	oldReplica := rotGetKey(t, w.sp, replicaKey) // capture the old-KEK replica bytes

	opts := backup.RotateKEKOptions{
		OldKEKRef: "test:old", OldKEK: oldKEK,
		NewKEKRef: "test:new", NewKEK: newKEK,
		Signer: w.signer, Verifier: w.verifier,
	}
	if _, err := backup.RotateKEK(context.Background(), w.sp, opts); err != nil {
		t.Fatalf("first rotate: %v", err)
	}

	// Strand the replica: revert ONLY the replica to its old-KEK bytes.
	rotPutKey(t, w.sp, replicaKey, oldReplica)
	if got := rotReplicaKEKRef(t, w.sp, replicaKey); got != "test:old" {
		t.Fatalf("setup: replica KEKRef = %q, want test:old (stranded state)", got)
	}

	// Re-run: must HEAL the replica (not skip the manifest as already-rotated).
	res, err := backup.RotateKEK(context.Background(), w.sp, opts)
	if err != nil {
		t.Fatalf("resume rotate: %v", err)
	}
	if res.ReplicaFailures != 0 {
		t.Errorf("ReplicaFailures = %d, want 0", res.ReplicaFailures)
	}
	if got := rotReplicaKEKRef(t, w.sp, replicaKey); got != "test:new" {
		t.Errorf("replica KEKRef = %q after resume, want test:new — replica stranded (data-loss path)", got)
	}
}

// rotateRetSP records SetRetention calls so a test can assert a rotated
// manifest (primary + replica) is re-locked on a WORM repo.
type rotateRetSP struct {
	storage.StoragePlugin
	keys map[string]storage.WORMMode
}

func (s *rotateRetSP) SetRetention(ctx context.Context, key string, until time.Time, mode storage.WORMMode) error {
	if s.keys == nil {
		s.keys = map[string]storage.WORMMode{}
	}
	s.keys[key] = mode
	return s.StoragePlugin.SetRetention(ctx, key, until, mode)
}

// TestRotateKEK_AppliesWORMLock pins the fix: a KEK rotation rewrites the
// manifest (and its replica) via tmp+rename with no retention; on a
// compliance repo those rewritten copies must be re-locked, not left
// deletable.
func TestRotateKEK_AppliesWORMLock(t *testing.T) {
	w := setupRotateWorld(t)
	oldKEK := mkKEK(t)
	newKEK := mkKEK(t)
	w.commitEncrypted(t, "db1", "db1.full.aaa", oldKEK, "test:old", 1)

	rec := &rotateRetSP{StoragePlugin: w.sp}
	until := time.Now().Add(time.Hour).UTC()
	res, err := backup.RotateKEK(context.Background(), rec, backup.RotateKEKOptions{
		OldKEKRef:     "test:old",
		OldKEK:        oldKEK,
		NewKEKRef:     "test:new",
		NewKEK:        newKEK,
		Signer:        w.signer,
		Verifier:      w.verifier,
		RetainUntil:   until,
		RetentionMode: storage.WORMCompliance,
	})
	if err != nil {
		t.Fatalf("RotateKEK: %v", err)
	}
	if res.Rotated != 1 {
		t.Fatalf("Rotated = %d, want 1 (failures: %+v)", res.Rotated, res.Failures)
	}

	primaryKey := backup.PrimaryPath("db1", "db1.full.aaa")
	replicaKey := backup.ReplicaPath("db1.full.aaa")
	if rec.keys[primaryKey] != storage.WORMCompliance {
		t.Errorf("rotated primary not WORM-locked; got mode %q for %s", rec.keys[primaryKey], primaryKey)
	}
	if rec.keys[replicaKey] != storage.WORMCompliance {
		t.Errorf("rotated replica not WORM-locked; got mode %q for %s", rec.keys[replicaKey], replicaKey)
	}
}
