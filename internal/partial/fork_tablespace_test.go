package partial_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/partial"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

type tsFile struct {
	oid  uint32
	path string
	body string
}

// commitTyped commits a signed manifest of the given type whose files
// may carry a TablespaceOID.
func (w *partialWorld) commitTyped(t *testing.T, backupID string, typ backup.BackupType, parent string, files []tsFile) {
	t.Helper()
	cas := casdefault.New(w.sp)
	var entries []backup.FileEntry
	for _, f := range files {
		info, err := cas.PutChunk(context.Background(), []byte(f.body))
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, backup.FileEntry{
			Path: f.path, Size: int64(len(f.body)), Mode: 0o600, TablespaceOID: f.oid,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: int64(len(f.body))}},
		})
	}
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: backupID, Deployment: "db1", Tenant: "default",
		Type: typ, ParentBackupID: parent, PGVersion: 17,
		SystemIdentifier: "7000000000000000001", StartLSN: "0/3000028", StopLSN: "0/30001A0",
		Timeline: 1, BackupLabel: "START WAL LOCATION: 0/3000028\n",
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}, {OID: 16500, Location: "/srv/ts1"}},
		Files:       entries,
	}
	if typ == backup.BackupTypeIncremental {
		m.PGBackupManifest = []byte(`{"PostgreSQL-Backup-Manifest-Version":1,"Files":[],"Incremental":true}`)
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit %s: %v", backupID, err)
	}
}

func usersMap(path string) map[string]partial.Relfilenode {
	return map[string]partial.Relfilenode{"public.users": {Schema: "public", Table: "users", Qualified: "public.users", Path: path}}
}

// Regression (H46): in an incremental backup a relation's main fork is
// stored as INCREMENTAL.<relfilenode> (only the changed blocks, to be
// combined with the chain), while the _fsm/_vm forks keep their plain
// names. `partial restore` extracted just those two forks and reported
// the table as extracted. It must refuse incremental backups outright.
func TestPartialRestore_RefusesIncrementalBackup(t *testing.T) {
	w := setupPartialWorld(t)
	w.commitTyped(t, "db1.full.a", backup.BackupTypeFull, "", []tsFile{{0, "base/16384/2619", "users-page-0"}})
	w.commitTyped(t, "db1.incremental.b", backup.BackupTypeIncremental, "db1.full.a", []tsFile{
		{0, "base/16384/INCREMENTAL.2619", "changed-blocks"},
		{0, "base/16384/2619_fsm", "fsm"},
		{0, "base/16384/2619_vm", "vm"},
	})
	target := filepath.Join(t.TempDir(), "extract")
	res, err := partial.Restore(context.Background(), partial.RestoreOptions{
		RepoURL: w.repoURL, Deployment: "db1", BackupID: "db1.incremental.b", Verifier: w.verifier,
		Tables: []string{"public.users"}, RelfilenodeMap: usersMap("base/16384/2619"), TargetDir: target,
	})
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "partial.incremental_unsupported" {
		t.Fatalf("want partial.incremental_unsupported, got err=%v res=%+v", err, res)
	}
	if _, serr := os.Stat(filepath.Join(target, "base/16384/2619_fsm")); serr == nil {
		t.Errorf("forks were extracted from an incremental backup")
	}
}

// Regression (H46): a table whose main fork is absent must never be
// reported as extracted, even if its _fsm/_vm forks are present.
func TestPartialRestore_MainForkMissingIsNotInBackup(t *testing.T) {
	w := setupPartialWorld(t)
	w.commitTyped(t, "db1.full.a", backup.BackupTypeFull, "", []tsFile{
		{0, "base/16384/2619_fsm", "fsm"},
		{0, "base/16384/2619_vm", "vm"},
	})
	res, err := partial.Restore(context.Background(), partial.RestoreOptions{
		RepoURL: w.repoURL, Deployment: "db1", BackupID: "db1.full.a", Verifier: w.verifier,
		Tables: []string{"public.users"}, RelfilenodeMap: usersMap("base/16384/2619"),
		TargetDir: filepath.Join(t.TempDir(), "extract"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.NotInBackup) != 1 || res.NotInBackup[0] != "public.users" {
		t.Fatalf("NotInBackup = %v, want [public.users] (no main fork)", res.NotInBackup)
	}
}

// Regression (LOW): tables in a non-default tablespace could never be
// extracted: pg_relation_filepath says pg_tblspc/<oid>/PG_…/<db>/<rfn>
// while the manifest stores the path relative to the tablespace root
// plus TablespaceOID, and indexFiles ignored the OID.
func TestPartialRestore_NonDefaultTablespaceTable(t *testing.T) {
	w := setupPartialWorld(t)
	w.commitTyped(t, "db1.full.a", backup.BackupTypeFull, "", []tsFile{
		{16500, "PG_17_202406281/16384/2619", "ts-users-page-0"},
		{16500, "PG_17_202406281/16384/2619_vm", "ts-users-vm"},
		{0, "base/16384/2619", "NOT-the-tablespace-file"},
	})
	target := filepath.Join(t.TempDir(), "extract")
	res, err := partial.Restore(context.Background(), partial.RestoreOptions{
		RepoURL: w.repoURL, Deployment: "db1", BackupID: "db1.full.a", Verifier: w.verifier,
		Tables: []string{"public.users"}, RelfilenodeMap: usersMap("pg_tblspc/16500/PG_17_202406281/16384/2619"),
		TargetDir: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.NotInBackup) != 0 || res.FilesWritten != 2 {
		t.Fatalf("FilesWritten=%d NotInBackup=%v, want 2 files and nothing missing", res.FilesWritten, res.NotInBackup)
	}
	got, err := os.ReadFile(filepath.Join(target, "pg_tblspc/16500/PG_17_202406281/16384/2619"))
	if err != nil || string(got) != "ts-users-page-0" {
		t.Fatalf("tablespace heap at its pg_relation_filepath: %q, %v", got, err)
	}
}
