package backup_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// TestManifestStore_RejectsTraversalIdentifiers pins input-validation
// audit #1: the read / delete / tombstone entry points reject a
// deployment or backup ID that would escape or splinter the storage-key
// hierarchy, instead of building `manifests/../...` style keys from
// untrusted API/CLI input.
func TestManifestStore_RejectsTraversalIdentifiers(t *testing.T) {
	store, _, _, verifier := newStore(t)
	ctx := context.Background()

	cases := []struct {
		dep, id string
	}{
		{"..", "good"},
		{"a/../b", "good"},
		{"db1/evil", "good"},
		{"db1", ".."},
		{"db1", "../../etc/passwd"},
		{"db1", "a/b"},
		{"db1", "a\\b"},
		{"db1\x00", "good"},
		{"db1", "x\ninjected"},
		{"", "good"},
		{"db1", ""},
	}
	for _, c := range cases {
		if _, err := store.Read(ctx, c.dep, c.id, verifier); err == nil {
			t.Errorf("Read(%q, %q) must be rejected", c.dep, c.id)
		}
		if err := store.SoftDelete(ctx, c.dep, c.id, "manual", "x"); err == nil {
			t.Errorf("SoftDelete(%q, %q) must be rejected", c.dep, c.id)
		}
		if _, err := store.IsTombstoned(ctx, c.dep, c.id); err == nil {
			t.Errorf("IsTombstoned(%q, %q) must be rejected", c.dep, c.id)
		}
	}
}

// TestManifestStore_AcceptsLegitimateIdentifiers: the validation must not
// reject the IDs the backup pipeline actually generates (which contain
// dots) or normal deployment names. Round-trips through a real Commit/Read.
func TestManifestStore_AcceptsLegitimateIdentifiers(t *testing.T) {
	store, _, signer, verifier := newStore(t)
	ctx := context.Background()

	m := sampleManifest()
	m.Deployment = "prod-db_1"
	m.BackupID = "prod-db_1.full.20260428T120000Z.a1b2"
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("legit manifest must commit: %v", err)
	}
	if _, err := store.Read(ctx, m.Deployment, m.BackupID, verifier); err != nil {
		t.Fatalf("legit manifest must read back: %v", err)
	}
}

// TestValidateIdentifiers_ExportedHelpers covers the exported validators
// the API/CLI boundaries use: legitimate values pass, traversal/control
// inputs are rejected.
func TestValidateIdentifiers_ExportedHelpers(t *testing.T) {
	good := []string{
		"db1", "prod-db_1", "tenant1",
		"db1.full.20260428T120000Z.a1b2", // backup-ID shape (dots ok)
	}
	for _, s := range good {
		if err := backup.ValidateBackupID(s); err != nil {
			t.Errorf("ValidateBackupID(%q) should pass; got %v", s, err)
		}
	}
	bad := []string{"", ".", "..", "a/b", "a\\b", "../x", "x/..", "a\x00b", "a\nb", "a\x7fb"}
	for _, s := range bad {
		if err := backup.ValidateBackupID(s); err == nil {
			t.Errorf("ValidateBackupID(%q) should be rejected", s)
		}
		if err := backup.ValidateDeployment(s); err == nil {
			t.Errorf("ValidateDeployment(%q) should be rejected", s)
		}
	}
	// "." and "/" must specifically be reported as path issues.
	if err := backup.ValidateDeployment(".."); err == nil || !strings.Contains(err.Error(), "reserved path component") {
		t.Errorf("`..` should be flagged as a reserved path component; got %v", err)
	}
}
