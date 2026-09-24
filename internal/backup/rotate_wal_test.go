package backup_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// plantWALSegment writes an encrypted WAL segment manifest whose own
// envelope wraps dek under kek with kekRef — the shape wal push /
// wal stream commit (issue #106).
func plantWALSegment(t *testing.T, sp storage.StoragePlugin, deployment, segName string, kek, dek [encryption.KeyLen]byte, kekRef string) string {
	t.Helper()
	wrapped, err := encryption.Wrap(kek, dek)
	if err != nil {
		t.Fatal(err)
	}
	m := &walsink.SegmentManifest{
		Schema:           walsink.Schema,
		Deployment:       deployment,
		SystemIdentifier: "7000000000000000001",
		Timeline:         1,
		SegmentName:      segName,
		SegmentSize:      walsink.SegmentSize,
		CreatedAt:        time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		Encryption: &walsink.EncryptionInfo{
			Scheme:          "aes-256-gcm",
			KEKRef:          kekRef,
			WrappedDEK:      base64.StdEncoding.EncodeToString(wrapped),
			EnvelopeVersion: 1,
		},
	}
	body, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	key := walsink.SegmentPath(deployment, 1, segName)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	return key
}

func readWALSegment(t *testing.T, sp storage.StoragePlugin, key string) *walsink.SegmentManifest {
	t.Helper()
	m, err := walsink.ParseSegmentManifest(rotGetKey(t, sp, key))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// KEK rotation used to rewrap backup manifests and the shared-DEK slot
// but NEVER the WAL segment manifests, which carry their own envelope
// and are unwrapped per segment by `wal fetch`. The local keystore maps
// every local:* ref to the single kek.bin, so the moment the operator
// installed the new KEK every pre-rotation segment became undecryptable
// — PITR past the base backup was impossible. Rotation must rewrap them
// too, and a re-run must count them as already rotated.
func TestRotateKEK_RewrapsWALSegmentManifests(t *testing.T) {
	w := setupRotateWorld(t)
	ctx := context.Background()
	oldKEK, newKEK := mkKEK(t), mkKEK(t)
	dek := mkKEK(t)

	w.commitEncrypted(t, "db1", "db1.full.aaa", oldKEK, "local:default", 1)
	k1 := plantWALSegment(t, w.sp, "db1", "000000010000000000000001", oldKEK, dek, "local:default")
	k2 := plantWALSegment(t, w.sp, "db1", "000000010000000000000002", oldKEK, dek, "local:default")
	// WAL-first deployment: segments but no base backup at all.
	k3 := plantWALSegment(t, w.sp, "walonly", "000000010000000000000007", oldKEK, dek, "local:default")
	// Another tenant's segment must be left alone.
	otherKEK := mkKEK(t)
	k4 := plantWALSegment(t, w.sp, "db1", "000000010000000000000003", otherKEK, dek, "tenant:other")

	opts := backup.RotateKEKOptions{
		OldKEKRef: "local:default", OldKEK: oldKEK,
		NewKEKRef: "local:v2", NewKEK: newKEK,
		Signer: w.signer, Verifier: w.verifier,
	}

	// Dry-run reports the plan without touching anything.
	dry := opts
	dry.DryRun = true
	res, err := backup.RotateKEK(ctx, w.sp, dry)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if res.WALRotated != 3 || res.WALSkippedDifferentKEK != 1 || res.WALFailed != 0 {
		t.Fatalf("dry-run WAL plan: rotated=%d different=%d failed=%d, want 3/1/0",
			res.WALRotated, res.WALSkippedDifferentKEK, res.WALFailed)
	}
	if got := readWALSegment(t, w.sp, k1); got.Encryption.KEKRef != "local:default" {
		t.Fatalf("dry-run rewrote a segment manifest (ref=%q)", got.Encryption.KEKRef)
	}

	res, err = backup.RotateKEK(ctx, w.sp, opts)
	if err != nil {
		t.Fatalf("RotateKEK: %v", err)
	}
	if res.WALConsidered != 4 || res.WALRotated != 3 || res.WALSkippedDifferentKEK != 1 || res.WALFailed != 0 {
		t.Fatalf("WAL pass: considered=%d rotated=%d different=%d failed=%d (%+v), want 4/3/1/0",
			res.WALConsidered, res.WALRotated, res.WALSkippedDifferentKEK, res.WALFailed, res.Failures)
	}

	for _, k := range []string{k1, k2, k3} {
		got := readWALSegment(t, w.sp, k)
		if got.Encryption.KEKRef != "local:v2" {
			t.Errorf("%s: KEKRef=%q, want local:v2", k, got.Encryption.KEKRef)
		}
		wrapped, _ := base64.StdEncoding.DecodeString(got.Encryption.WrappedDEK)
		d, err := encryption.Unwrap(newKEK, wrapped)
		if err != nil {
			t.Errorf("%s: NEW KEK cannot unwrap the segment DEK after rotation: %v — PITR is impossible once the old KEK is retired", k, err)
			continue
		}
		if d != dek {
			t.Errorf("%s: segment DEK changed across rotation", k)
		}
	}
	if got := readWALSegment(t, w.sp, k4); got.Encryption.KEKRef != "tenant:other" {
		t.Errorf("other tenant's segment was touched: ref=%q", got.Encryption.KEKRef)
	}

	// Resume: a re-run counts them as already rotated and fails nothing.
	res, err = backup.RotateKEK(ctx, w.sp, opts)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if res.WALAlreadyRotated != 3 || res.WALRotated != 0 || res.WALFailed != 0 {
		t.Fatalf("re-run WAL pass: already=%d rotated=%d failed=%d, want 3/0/0",
			res.WALAlreadyRotated, res.WALRotated, res.WALFailed)
	}

	// A segment pushed AFTER the operator installed the new kek.bin still
	// carries the fixed local:default ref but is wrapped under the NEW
	// KEK. A resumed rotation must treat it as done, not as a failure.
	plantWALSegment(t, w.sp, "db1", "000000010000000000000009", newKEK, dek, "local:default")
	res, err = backup.RotateKEK(ctx, w.sp, opts)
	if err != nil {
		t.Fatalf("post-install re-run: %v", err)
	}
	if res.WALFailed != 0 || res.WALAlreadyRotated != 4 {
		t.Fatalf("post-install re-run: already=%d failed=%d (%+v), want 4/0",
			res.WALAlreadyRotated, res.WALFailed, res.Failures)
	}
}

// A segment whose rewrite the backend refuses (WORM object lock) must
// fail the pass loudly and keep the shared-DEK migration from declaring
// the rotation complete.
func TestRotateKEK_WALRewriteRefusedFailsLoudly(t *testing.T) {
	w := setupRotateWorld(t)
	ctx := context.Background()
	oldKEK, newKEK, dek := mkKEK(t), mkKEK(t), mkKEK(t)
	key := plantWALSegment(t, w.sp, "db1", "000000010000000000000001", oldKEK, dek, "local:default")

	sp := &refusePutSP{StoragePlugin: w.sp, refuse: key}
	res, err := backup.RotateKEK(ctx, sp, backup.RotateKEKOptions{
		OldKEKRef: "local:default", OldKEK: oldKEK,
		NewKEKRef: "local:v2", NewKEK: newKEK,
		Signer: w.signer, Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("RotateKEK: %v", err)
	}
	if res.WALFailed != 1 || len(res.Failures) != 1 || res.Failures[0].Key != key {
		t.Fatalf("WALFailed=%d failures=%+v, want exactly the refused segment", res.WALFailed, res.Failures)
	}
	if res.SharedDEKMigrated {
		t.Fatal("shared-DEK slot migrated although a WAL segment still needs the old KEK")
	}
}

type refusePutSP struct {
	storage.StoragePlugin
	refuse string
}

func (s *refusePutSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if key == s.refuse {
		return storage.PutResult{}, errWORMLocked
	}
	return s.StoragePlugin.Put(ctx, key, r, opts)
}

var errWORMLocked = &wormErr{}

type wormErr struct{}

func (*wormErr) Error() string { return "object is WORM-locked" }

// A tombstoned (soft-deleted, still undeletable) backup still holds a
// DEK wrapped under the old KEK. Rotation used to skip it and then say
// the old KEK could be retired — after which `backup undelete` resurrects
// a backup nobody can decrypt.
func TestRotateKEK_IncludesTombstonedManifests(t *testing.T) {
	w := setupRotateWorld(t)
	ctx := context.Background()
	oldKEK, newKEK := mkKEK(t), mkKEK(t)
	w.commitEncrypted(t, "db1", "db1.full.aaa", oldKEK, "test:old", 1)
	if err := w.store.SoftDelete(ctx, "db1", "db1.full.aaa", "test", "rotation test"); err != nil {
		t.Fatal(err)
	}

	res, err := backup.RotateKEK(ctx, w.sp, backup.RotateKEKOptions{
		OldKEKRef: "test:old", OldKEK: oldKEK,
		NewKEKRef: "test:new", NewKEK: newKEK,
		Signer: w.signer, Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rotated != 1 || res.TombstonedRotated != 1 {
		t.Fatalf("rotated=%d tombstoned_rotated=%d, want 1/1 — the soft-deleted backup kept the old wrap", res.Rotated, res.TombstonedRotated)
	}
	m, dead, err := w.store.ReadIncludingTombstoned(ctx, "db1", "db1.full.aaa", w.verifier)
	if err != nil {
		t.Fatal(err)
	}
	if !dead {
		t.Error("rotation must not un-tombstone the backup")
	}
	if m.Encryption.KEKRef != "test:new" {
		t.Errorf("tombstoned manifest KEKRef=%q, want test:new", m.Encryption.KEKRef)
	}
}

// A local backup taken after the new kek.bin was installed carries the
// fixed local:default ref but is wrapped under the NEW key. A resumed
// rotation must count it as done; it used to fail it, so the rotation
// could never exit clean again.
func TestRotateKEK_PostInstallBackupIsAlreadyRotated(t *testing.T) {
	w := setupRotateWorld(t)
	oldKEK, newKEK := mkKEK(t), mkKEK(t)
	w.commitEncrypted(t, "db1", "db1.full.bbb", newKEK, "local:default", 2)
	res, err := backup.RotateKEK(context.Background(), w.sp, backup.RotateKEKOptions{
		OldKEKRef: "local:default", OldKEK: oldKEK,
		NewKEKRef: "local:v2", NewKEK: newKEK,
		Signer: w.signer, Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 0 || res.AlreadyRotated != 1 {
		t.Fatalf("failed=%d already=%d (%+v), want 0/1", res.Failed, res.AlreadyRotated, res.Failures)
	}
}
