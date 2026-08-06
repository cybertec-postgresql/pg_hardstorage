package backup_test

// appendonly_test.go — publishing a manifest must not delete anything.
//
// Issue #45 came from an operator whose repository bucket is their
// anti-ransomware copy of record: versioning on, replication that only
// ever adds objects, a delete marker treated as an anomaly. Every
// manifest commit produced one, because publishing went through
// `<key>.tmp.<rand>` + RenameIfNotExists — which on S3 is HeadObject +
// CopyObject + DELETE.
//
// The commit path now uses a single conditional PUT where the backend
// supports one. This asserts the consequence rather than the
// mechanism: a full ManifestStore.Commit, primary and replica, issues
// no deletes at all.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// deleteRecordingSP records every Delete performed through it.
type deleteRecordingSP struct {
	storage.StoragePlugin
	mu      sync.Mutex
	deletes []string
}

func (d *deleteRecordingSP) Delete(ctx context.Context, key string) error {
	d.mu.Lock()
	d.deletes = append(d.deletes, key)
	d.mu.Unlock()
	return d.StoragePlugin.Delete(ctx, key)
}

func (d *deleteRecordingSP) Deletes() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.deletes...)
}

// TestManifestCommit_IssuesNoDeletes is the property the issue asked
// for, stated as an assertion.
func TestManifestCommit_IssuesNoDeletes(t *testing.T) {
	_, sp, signer, _ := newStore(t)
	rec := &deleteRecordingSP{StoragePlugin: sp}
	if !rec.Capabilities().ConditionalPut {
		t.Skip("fixture backend cannot commit conditionally; staging — and its delete — " +
			"is the only option there. The fallback path is covered by " +
			"TestCommitExclusive_FallbackStillWorksWithoutConditionalPut and " +
			"TestCommitExclusive_FallbackCleansUpItsStagingObject")
	}
	store := backup.NewManifestStore(rec)

	m := sampleManifest()
	m.Deployment = "db1"
	m.BackupID = "db1.full.appendonly"
	m.Type = backup.BackupTypeFull
	m.ParentBackupID = ""

	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if d := rec.Deletes(); len(d) != 0 {
		t.Errorf("committing one manifest issued %d delete(s):\n  %s\n\n"+
			"On a versioned bucket each is a delete marker, and a repository kept as an "+
			"anti-ransomware copy of record treats those as an anomaly — which is why WAL "+
			"archiving could not be adopted there at all (issue #45)",
			len(d), strings.Join(d, "\n  "))
	}
}
