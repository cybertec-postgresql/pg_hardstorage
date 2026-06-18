package cli_test

import (
	"context"
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// commitWithFilesCLI plants a real (signed) backup manifest with
// the given file paths. Each path gets one synthetic chunk planted
// in the CAS and a matching FileEntry referencing it.
//
// We reuse the readWorld signer so the CLI's loadVerifier accepts
// what we wrote. StoppedAt is set to a deterministic-but-monotonic
// time per call so latest-backup resolution can pick the most
// recent commit.
var partialTestCounter int

func (w *readWorld) commitWithFilesCLI(t *testing.T, deployment, backupID string, files []cliFileSpec) {
	t.Helper()
	partialTestCounter++
	stoppedAt := time.Date(2026, 4, 30, 0, partialTestCounter, 0, 0, time.UTC)
	startedAt := stoppedAt.Add(-30 * time.Second)
	cas := casdefault.New(w.sp)
	var entries []backup.FileEntry
	for _, f := range files {
		var refs []backup.ChunkRef
		var size int64
		for _, body := range f.chunks {
			info, err := cas.PutChunk(context.Background(), body)
			if err != nil {
				t.Fatalf("PutChunk: %v", err)
			}
			refs = append(refs, backup.ChunkRef{
				Hash:   info.Hash,
				Offset: size,
				Len:    int64(len(body)),
			})
			size += int64(len(body))
		}
		entries = append(entries, backup.FileEntry{
			Path:   f.path,
			Size:   size,
			Mode:   0o600,
			Chunks: refs,
		})
	}
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
		StartedAt:        startedAt,
		StoppedAt:        stoppedAt,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:            entries,
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

type cliFileSpec struct {
	path   string
	chunks [][]byte
}

// writeRelfilenodeMap writes a minimal JSON map at path that the
// CLI's loadRelfilenodeMap can parse.
func writeRelfilenodeMap(t *testing.T, mapPath string, entries map[string]map[string]any) {
	t.Helper()
	body, err := stdjson.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mapPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPartialRestore_RequiresRepo: the cobra-level missing-flag
// case for --repo.
func TestPartialRestore_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, stderr, exit := runCLI(t,
		"partial", "restore", "db1",
		"--tables", "public.users",
		"--target", "/tmp/x",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --repo should exit Misuse; got %d\nstderr=%s", exit, stderr)
	}
}

// TestPartialRestore_RequiresTables: --tables is mandatory.
func TestPartialRestore_RequiresTables(t *testing.T) {
	w := newReadWorld(t)
	_, stderr, exit := runCLI(t,
		"partial", "restore", "db1",
		"--repo", w.repoURL,
		"--target", "/tmp/x",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --tables should exit Misuse; got %d\nstderr=%s", exit, stderr)
	}
}

// TestPartialRestore_RequiresResolution: --pg-connection or
// --relfilenode-map must be set.
func TestPartialRestore_RequiresResolution(t *testing.T) {
	w := newReadWorld(t)
	_, stderr, exit := runCLI(t,
		"partial", "restore", "db1",
		"--repo", w.repoURL,
		"--tables", "public.users",
		"--target", "/tmp/x",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing resolution should exit Misuse; got %d\nstderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "--pg-connection") {
		t.Errorf("error should mention --pg-connection: %s", stderr)
	}
}

// TestPartialRestore_RejectsBothResolutionPaths: --pg-connection
// and --relfilenode-map together is a usage error.
func TestPartialRestore_RejectsBothResolutionPaths(t *testing.T) {
	w := newReadWorld(t)
	mapPath := filepath.Join(t.TempDir(), "map.json")
	writeRelfilenodeMap(t, mapPath, map[string]map[string]any{
		"public.users": {"qualified": "public.users", "path": "base/16384/2619"},
	})
	_, stderr, exit := runCLI(t,
		"partial", "restore", "db1",
		"--repo", w.repoURL,
		"--tables", "public.users",
		"--target", filepath.Join(t.TempDir(), "out"),
		"--pg-connection", "postgres:///x",
		"--relfilenode-map", mapPath,
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("both flags should exit Misuse; got %d\nstderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("expected mutually-exclusive: %s", stderr)
	}
}

// TestPartialRestore_HappyPath: plant a backup with two tables'
// heap files, run partial restore for one of them via
// --relfilenode-map, assert only that table's files land at
// --target.
func TestPartialRestore_HappyPath(t *testing.T) {
	w := newReadWorld(t)
	w.commitWithFilesCLI(t, "db1", "db1.full.partial", []cliFileSpec{
		{"base/16384/2619", [][]byte{[]byte("users-heap")}},
		{"base/16384/2619_vm", [][]byte{[]byte("users-vm")}},
		{"base/16384/2620", [][]byte{[]byte("orders-heap")}},
	})

	target := filepath.Join(t.TempDir(), "extract")
	mapPath := filepath.Join(t.TempDir(), "rfn.json")
	writeRelfilenodeMap(t, mapPath, map[string]map[string]any{
		"public.users": {
			"qualified": "public.users",
			"schema":    "public",
			"table":     "users",
			"path":      "base/16384/2619",
		},
	})

	stdout, stderr, exit := runCLI(t,
		"partial", "restore", "db1",
		"--repo", w.repoURL,
		"--backup", "db1.full.partial",
		"--tables", "public.users",
		"--target", target,
		"--relfilenode-map", mapPath,
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("partial restore: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}

	// users files are present.
	for _, p := range []string{"base/16384/2619", "base/16384/2619_vm"} {
		if _, err := os.Stat(filepath.Join(target, p)); err != nil {
			t.Errorf("expected %q present: %v", p, err)
		}
	}
	// orders file is NOT.
	if _, err := os.Stat(filepath.Join(target, "base/16384/2620")); err == nil {
		t.Error("orders heap should NOT have been extracted")
	}
}

// TestPartialRestore_LatestBackupResolution: --backup latest (the
// default) walks the manifest store and picks the newest. We plant
// two backups; the newer one's tables should be the ones extracted.
func TestPartialRestore_LatestBackupResolution(t *testing.T) {
	w := newReadWorld(t)
	w.commitWithFilesCLI(t, "db1", "db1.full.older", []cliFileSpec{
		{"base/16384/2619", [][]byte{[]byte("older")}},
	})
	w.commitWithFilesCLI(t, "db1", "db1.full.zzz_newer", []cliFileSpec{
		{"base/16384/2619", [][]byte{[]byte("newer")}},
	})

	target := filepath.Join(t.TempDir(), "extract")
	mapPath := filepath.Join(t.TempDir(), "rfn.json")
	writeRelfilenodeMap(t, mapPath, map[string]map[string]any{
		"public.users": {"qualified": "public.users", "path": "base/16384/2619"},
	})

	stdout, _, exit := runCLI(t,
		"partial", "restore", "db1",
		"--repo", w.repoURL,
		"--tables", "public.users",
		"--target", target,
		"--relfilenode-map", mapPath,
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	// Read what landed; it should be the newer body.
	body, err := os.ReadFile(filepath.Join(target, "base/16384/2619"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "newer" {
		t.Errorf("extracted older backup; expected 'newer' body, got %q", body)
	}
}

// TestPartialRestore_TextRender confirms the operator-friendly
// text output has the post-restore "next step" hint.
func TestPartialRestore_TextRender(t *testing.T) {
	w := newReadWorld(t)
	w.commitWithFilesCLI(t, "db1", "db1.full.tx", []cliFileSpec{
		{"base/16384/2619", [][]byte{[]byte("users")}},
	})
	target := filepath.Join(t.TempDir(), "extract")
	mapPath := filepath.Join(t.TempDir(), "rfn.json")
	writeRelfilenodeMap(t, mapPath, map[string]map[string]any{
		"public.users": {"qualified": "public.users", "path": "base/16384/2619"},
	})

	stdout, _, exit := runCLI(t,
		"partial", "restore", "db1",
		"--repo", w.repoURL,
		"--backup", "db1.full.tx",
		"--tables", "public.users",
		"--target", target,
		"--relfilenode-map", mapPath,
		"-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"partial restore",
		"Tables:",
		"Files written:",
		"Next step:",
		"pg_dump",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text render missing %q:\n%s", want, stdout)
		}
	}
}

// TestPartialRestore_NotFoundTable_PropagatesNotFound: a table
// missing from the relfilenode map shows up under not_found in
// the result body. The run still succeeds (exit 0).
func TestPartialRestore_NotFoundTable_PropagatesNotFound(t *testing.T) {
	w := newReadWorld(t)
	w.commitWithFilesCLI(t, "db1", "db1.full.nf", []cliFileSpec{
		{"base/16384/2619", [][]byte{[]byte("present")}},
	})
	target := filepath.Join(t.TempDir(), "extract")
	mapPath := filepath.Join(t.TempDir(), "rfn.json")
	writeRelfilenodeMap(t, mapPath, map[string]map[string]any{
		"public.users": {"qualified": "public.users", "path": "base/16384/2619"},
	})

	stdout, _, exit := runCLI(t,
		"partial", "restore", "db1",
		"--repo", w.repoURL,
		"--backup", "db1.full.nf",
		"--tables", "public.users,public.does_not_exist",
		"--target", target,
		"--relfilenode-map", mapPath,
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"public.does_not_exist"`) {
		t.Errorf("expected not_found mention:\n%s", stdout)
	}
	// Files for the present table should still have landed.
	if _, err := os.Stat(filepath.Join(target, "base/16384/2619")); err != nil {
		t.Errorf("present table should still extract: %v", err)
	}
}

// Used by the test helper but ensure it doesn't shadow the
// production `_ = repo` dependency (no-op here).
var _ = repo.HSREPOFilename
