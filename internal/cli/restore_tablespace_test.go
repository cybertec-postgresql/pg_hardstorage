package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// commitVerifiableBackupWithTablespaceMap commits a manifest
// whose TablespaceMap body is the supplied content. Mirror of
// commitVerifiableBackup with the extra field populated.
func commitVerifiableBackupWithTablespaceMap(t *testing.T, w *readWorld, deployment string, idx int, body []byte, tsmapBody string) string {
	t.Helper()
	cas := repo.NewCAS(w.sp)
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatalf("put chunk: %v", err)
	}
	ts := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Add(time.Duration(idx) * time.Minute)
	id := deployment + ".tsmap." + ts.Format("20060102T150405Z")
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        180000,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        ts,
		StoppedAt:        ts.Add(30 * time.Second),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		TablespaceMap:    tsmapBody,
		Files: []backup.FileEntry{
			{Path: "PG_VERSION", Size: int64(len(body)), Mode: 0o600,
				Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: int64(len(body))}}},
		},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

// TestRestore_TablespaceMapping_RewritesAndSurfacesInBody:
// end-to-end CLI happy path. Restore with --tablespace-mapping,
// assert (a) tablespace_map on disk has the rewritten path,
// (b) the result body's tablespace_remap surfaces the
// operator's mapping.
func TestRestore_TablespaceMapping_RewritesAndSurfacesInBody(t *testing.T) {
	w := newReadWorld(t)
	body := []byte("17\n")
	mapBody := "1663 /mnt/ssd/ts_fast\n1664 /mnt/hdd/ts_archive\n"
	id := commitVerifiableBackupWithTablespaceMap(t, w, "db1", 0, body, mapBody)
	target := t.TempDir() + "/restored"

	stdout, _, exit := runCLI(t, "restore", "db1", id,
		"--repo", w.repoURL,
		"--target", target,
		"--tablespace-mapping", "/mnt/ssd/ts_fast=/var/lib/pg/ts_fast",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("restore exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"tablespace_remap"`,
		`"old": "/mnt/ssd/ts_fast"`,
		`"new": "/var/lib/pg/ts_fast"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("body missing %q:\n%s", want, stdout)
		}
	}

	// On disk: rewritten path landed.
	got, err := os.ReadFile(filepath.Join(target, "tablespace_map"))
	if err != nil {
		t.Fatalf("read tablespace_map: %v", err)
	}
	if !strings.Contains(string(got), "1663 /var/lib/pg/ts_fast\n") {
		t.Errorf("expected rewritten path; got %q", got)
	}
	if !strings.Contains(string(got), "1664 /mnt/hdd/ts_archive\n") {
		t.Errorf("non-mapped entry should be untouched; got %q", got)
	}
}

// TestRestore_TablespaceMapping_Repeatable: multiple
// --tablespace-mapping entries combine. CLI's StringArrayVar
// supports the repeatable shape.
func TestRestore_TablespaceMapping_Repeatable(t *testing.T) {
	w := newReadWorld(t)
	mapBody := "1 /a\n2 /b\n3 /c\n"
	id := commitVerifiableBackupWithTablespaceMap(t, w, "db1", 0, []byte("17\n"), mapBody)
	target := t.TempDir() + "/restored"

	_, _, exit := runCLI(t, "restore", "db1", id,
		"--repo", w.repoURL,
		"--target", target,
		"--tablespace-mapping", "/a=/A",
		"--tablespace-mapping", "/b=/B",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("repeated --tablespace-mapping: exit=%d", exit)
	}
	got, _ := os.ReadFile(filepath.Join(target, "tablespace_map"))
	want := "1 /A\n2 /B\n3 /c\n"
	if string(got) != want {
		t.Errorf("two-entry remap = %q; want %q", got, want)
	}
}

// TestRestore_TablespaceMapping_BadFormat_RefusedAtUsage: a
// malformed --tablespace-mapping refuses with
// usage.bad_tablespace_mapping before any storage round-trip.
func TestRestore_TablespaceMapping_BadFormat_RefusedAtUsage(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackupWithTablespaceMap(t, w, "db1", 0, []byte("17\n"), "")
	target := t.TempDir() + "/restored"

	cases := []struct {
		name string
		flag string
		want string
	}{
		{"missing-equals", "/no-equals", "OLD=NEW"},
		{"relative-old", "ts=/var/x", "must be absolute"},
		{"relative-new", "/var/x=ts", "must be absolute"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, stderr, exit := runCLI(t, "restore", "db1", id,
				"--repo", w.repoURL,
				"--target", target,
				"--tablespace-mapping", c.flag,
				"-o", "json")
			if exit != int(output.ExitMisuse) {
				t.Errorf("exit=%d, want ExitMisuse", exit)
			}
			if !strings.Contains(stderr, "usage.bad_tablespace_mapping") {
				t.Errorf("expected usage.bad_tablespace_mapping:\n%s", stderr)
			}
			if !strings.Contains(stderr, c.want) {
				t.Errorf("expected message substring %q:\n%s", c.want, stderr)
			}
		})
	}
}

// TestRestore_TablespaceMapping_DefaultBodyShape_Unchanged:
// regression — without --tablespace-mapping, the result body
// must NOT include the tablespace_remap key (omitempty).
// 24-month JSON-compat regression.
func TestRestore_TablespaceMapping_DefaultBodyShape_Unchanged(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackupWithTablespaceMap(t, w, "db1", 0, []byte("17\n"), "")
	target := t.TempDir() + "/restored"

	stdout, _, exit := runCLI(t, "restore", "db1", id,
		"--repo", w.repoURL,
		"--target", target,
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("default restore: exit=%d", exit)
	}
	if strings.Contains(stdout, `"tablespace_remap"`) {
		t.Errorf("default body should not include tablespace_remap:\n%s", stdout)
	}
}

// TestRestore_TablespaceMapping_FlagDiscoverable: --help
// advertises --tablespace-mapping with the "repeatable" hint
// + the absolute-paths requirement.
func TestRestore_TablespaceMapping_FlagDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "restore", "--help")
	for _, want := range []string{
		"--tablespace-mapping",
		"OLDDIR",
		"NEWDIR",
		"repeatable",
		"absolute",
		"pg_combinebackup",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("restore --help missing %q:\n%s", want, stdout)
		}
	}
}
