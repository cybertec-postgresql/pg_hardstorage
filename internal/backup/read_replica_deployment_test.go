package backup_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// TestRead_ReplicaFallbackChecksDeployment pins bug 74: replicas are keyed by
// backupID alone (ReplicaPath ignores deployment), so a replica written for
// backupID under deployment "other" must NOT be served by Read for the same
// backupID under a DIFFERENT deployment — doing so would bypass the requested
// deployment's tombstone gate. Read must verify the replica's Deployment
// matches, mirroring EnsureReplica's identity guard.
func TestRead_ReplicaFallbackChecksDeployment(t *testing.T) {
	store, _, signer, verifier := newStore(t)
	ctx := context.Background()

	const sharedID = "shared.full.A"

	// Commit under deployment "other" — this writes both the primary
	// (manifests/other/backups/shared.full.A/manifest.json) AND the
	// backupID-keyed replica (manifests/_replicas/shared.full.A.manifest.json).
	mo := sampleManifest()
	mo.Deployment = "other"
	mo.BackupID = sharedID
	mo.Type = backup.BackupTypeFull
	mo.ParentBackupID = ""
	if err := store.Commit(ctx, mo, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit other: %v", err)
	}

	// There is NO backup with this ID under deployment "requested": its
	// primary manifest does not exist. The only object with the matching
	// replica key belongs to "other".
	//
	// Read("requested", sharedID) must return ErrNotFound — NOT the
	// "other" deployment's manifest served out of the shared-key replica.
	m, err := store.Read(ctx, "requested", sharedID, verifier)
	if err == nil {
		t.Fatalf("Read must not serve another deployment's manifest from the shared-key replica; got %+v", m)
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Read of a nonexistent backup should surface ErrNotFound (replica identity mismatch treated as not-present); got %v", err)
	}

	// Sanity: the legitimate read under the correct deployment still works.
	if _, err := store.Read(ctx, "other", sharedID, verifier); err != nil {
		t.Errorf("Read under the owning deployment should succeed; got %v", err)
	}
}
