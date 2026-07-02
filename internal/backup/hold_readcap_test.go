package backup_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// oversizedHoldSP returns an effectively unbounded body for the hold key,
// simulating a corrupt/oversized hold object. Every other key passes
// through to the wrapped plugin.
type oversizedHoldSP struct {
	storage.StoragePlugin
	holdKey string
}

func (s *oversizedHoldSP) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == s.holdKey {
		// A reader far larger than MaxManifestBytes. If GetHold used an
		// unbounded io.ReadAll this would attempt to allocate the whole
		// thing; the read cap makes it error out early instead.
		return io.NopCloser(io.LimitReader(neverEndingReader{}, int64(backup.MaxManifestBytes)+1024)), nil
	}
	return s.StoragePlugin.Get(ctx, key)
}

// neverEndingReader yields 'x' forever.
type neverEndingReader struct{}

func (neverEndingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// TestGetHold_ReadIsCapped pins bug 76: GetHold reads the hold body with a
// bounded read (readAllLimited/MaxManifestBytes), so a corrupt or oversized
// hold object errors out instead of OOMing every hold path.
func TestGetHold_ReadIsCapped(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	ctx := context.Background()

	m := sampleManifest()
	m.Deployment = "db1"
	m.BackupID = "db1.full.A"
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := store.PutHold(ctx, m.Deployment, m.BackupID, "ops", "hold"); err != nil {
		t.Fatalf("put hold: %v", err)
	}

	wrapped := &oversizedHoldSP{
		StoragePlugin: sp,
		holdKey:       backup.HoldPath(m.Deployment, m.BackupID),
	}
	capStore := backup.NewManifestStore(wrapped)

	_, err := capStore.GetHold(ctx, m.Deployment, m.BackupID)
	if err == nil {
		t.Fatal("GetHold on an oversized hold body must error (read cap), not slurp it unboundedly")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should mention the byte limit; got %v", err)
	}
}

// TestPutHoldUntil_RejectsUnsafeIdentifiers pins bug 76: PutHoldUntil runs
// the validateRef storage-ID injection check up front, mirroring the sibling
// tombstone paths (SoftDelete etc.). A traversal identifier is refused
// before any storage write.
func TestPutHoldUntil_RejectsUnsafeIdentifiers(t *testing.T) {
	store, _, _, _ := newStore(t)
	ctx := context.Background()

	cases := []struct {
		name       string
		deployment string
		backupID   string
	}{
		{"deployment traversal", "../evil", "db1.full.A"},
		{"backupID traversal", "db1", "../../etc/passwd"},
		{"backupID slash", "db1", "a/b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := store.PutHold(ctx, tc.deployment, tc.backupID, "ops", "reason")
			if err == nil {
				t.Fatalf("PutHold(%q,%q) with unsafe identifier must be rejected", tc.deployment, tc.backupID)
			}
			// Must be a validation refusal, NOT a not-found from an
			// attempted Stat on a mangled key.
			if errors.Is(err, storage.ErrNotFound) {
				t.Errorf("unsafe identifier should be rejected by validateRef, not surface as ErrNotFound: %v", err)
			}
		})
	}
}
