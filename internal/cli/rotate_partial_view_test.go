package cli_test

// rotate must refuse to run on a partial view of a deployment's
// manifests.
//
// The set loadDeploymentManifests returns IS the input to the retention
// decision, and retention DELETES. A manifest missing from that set is
// invisible to promoteChainParents, so its parent can be selected for
// deletion and the incremental that needed it becomes unrestorable —
// a partial view is how a retention pass destroys a chain it was never
// shown.
//
// The code already aborts. What was missing is that anything checked
// it: the comment above the abort claimed the opposite ("skip it but
// don't abort — operators want the rest of retention to proceed"),
// describing the unsafe behaviour as intended, with no test to
// contradict a maintainer who believed it.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func TestRotate_RefusesWhenAManifestWillNotVerify(t *testing.T) {
	repoURL := initRepoForTest(t)
	repoDir := strings.TrimPrefix(repoURL, "file://")

	// Three good manifests, oldest first.
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	stamps := []time.Time{base, base.AddDate(0, 0, 10), base.AddDate(0, 0, 20)}
	{
		_, sp, err := repo.Open(context.Background(), repoURL)
		if err != nil {
			t.Fatal(err)
		}
		store := backup.NewManifestStore(sp)
		p, err := paths.Resolve(paths.DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		signer, _, err := keystore.LoadOrGenerate(p.Keyring.Value)
		if err != nil {
			t.Fatal(err)
		}
		for _, ts := range stamps {
			id := "db1.full." + ts.UTC().Format("20060102T150405Z")
			m := &backup.Manifest{
				Schema: backup.Schema, BackupID: id, Deployment: "db1",
				Type: backup.BackupTypeFull, PGVersion: 170,
				SystemIdentifier: "7388123", StartLSN: "0/0", StopLSN: "0/0",
				Timeline: 1, StartedAt: ts.Add(-time.Minute), StoppedAt: ts,
				BackupLabel: "START WAL LOCATION: 0/0\n",
				Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
				Files:       []backup.FileEntry{},
			}
			if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
				t.Fatalf("commit %s: %v", id, err)
			}
		}
		sp.Close()
	}

	// Sanity: rotate works on the intact set.
	out, _, exit := runCLI(t, "rotate", "--repo", repoURL, "--keep-daily", "1", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("precondition: rotate on an intact repo failed: %s", out)
	}

	// Now corrupt ONE manifest body so it no longer verifies.
	var corrupted string
	err := filepath.Walk(filepath.Join(repoDir, "manifests"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || corrupted != "" {
			return nil
		}
		if filepath.Base(p) != "manifest.json" {
			return nil
		}
		body, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		// Flip a byte inside the signed content, leaving valid JSON.
		mangled := strings.Replace(string(body), `"system_identifier":"7388123"`,
			`"system_identifier":"7388124"`, 1)
		if mangled == string(body) {
			return nil
		}
		if werr := os.WriteFile(p, []byte(mangled), 0o644); werr != nil {
			return werr
		}
		corrupted = p
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if corrupted == "" {
		t.Fatal("could not corrupt a manifest; the on-disk layout changed")
	}

	// rotate must now REFUSE, not proceed on the remaining two.
	var stderr string
	out, stderr, exit = runCLI(t, "rotate", "--repo", repoURL, "--keep-daily", "1", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("rotate succeeded with one manifest unverifiable.\n\nstdout=%s\n\n"+
			"It then decided retention from a PARTIAL view: the missing manifest is "+
			"invisible to chain-parent promotion, so a parent it depends on can be "+
			"selected for deletion and the incremental becomes unrestorable.", out)
	}
	if !strings.Contains(stderr+out, "rotate.list_failed") {
		t.Errorf("refusal does not carry the rotate.list_failed code:\nstdout=%s\nstderr=%s", out, stderr)
	}
}
