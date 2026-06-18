package cli_test

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// commitPlantedManifests fabricates n manifests with synthetic
// StoppedAt values and commits them through the real ManifestStore.
// The keystore for the test is the same one the CLI would resolve at
// runtime (paths.Resolve), so signed-by-our-keypair-only invariants
// hold.
func commitPlantedManifests(t *testing.T, sp interface{}, deployment string, stamps []time.Time) {
	t.Helper()
	storagePlugin, ok := sp.(interface {
		// ManifestStore consumes a real StoragePlugin; we accept the
		// interface{} from initRepoForTest's storage handle by type-
		// asserting against the methods we use. This avoids importing
		// the storage package just for the type.
	})
	_ = storagePlugin
	_ = ok
	// In practice we pass *fs.Plugin via repo.Open, but the test
	// helper uses the stored interface so we re-walk through the
	// real ManifestStore wired to a freshly opened plugin.
}

func TestRotate_DryRunListsKeepAndDelete(t *testing.T) {
	repoURL := initRepoForTest(t)

	// Seed a few manifests via the real ManifestStore so signatures
	// round-trip cleanly.
	plant := func(stamps []time.Time) {
		_, sp, err := repo.Open(context.Background(), repoURL)
		if err != nil {
			t.Fatal(err)
		}
		defer sp.Close()
		store := backup.NewManifestStore(sp)

		p, err := paths.Resolve(paths.DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		signer, _, err := keystore.LoadOrGenerate(p.Keyring.Value)
		if err != nil {
			t.Fatal(err)
		}

		for i, ts := range stamps {
			id := "db1.full." + ts.UTC().Format("20060102T150405Z")
			m := &backup.Manifest{
				Schema:           backup.Schema,
				BackupID:         id,
				Deployment:       "db1",
				Type:             backup.BackupTypeFull,
				PGVersion:        170,
				SystemIdentifier: "7388123",
				StartLSN:         "0/0",
				StopLSN:          "0/0",
				Timeline:         1,
				StartedAt:        ts.Add(-time.Minute),
				StoppedAt:        ts,
				BackupLabel:      "START WAL LOCATION: 0/0\n",
				Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
				Files:            []backup.FileEntry{},
			}
			_ = i
			if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
				t.Fatalf("commit %s: %v", id, err)
			}
		}
	}

	// 5 backups, each 1 day apart, going back from a fixed reference.
	now := time.Now().UTC()
	stamps := []time.Time{
		now.Add(-1 * 24 * time.Hour),
		now.Add(-2 * 24 * time.Hour),
		now.Add(-3 * 24 * time.Hour),
		now.Add(-10 * 24 * time.Hour),
		now.Add(-30 * 24 * time.Hour),
	}
	plant(stamps)

	out, _, exit := runCmd(t,
		"rotate", "db1",
		"--repo", repoURL,
		"--policy", "gfs",
		"--keep-daily", "2",
		"--keep-weekly", "0",
		"--keep-monthly", "1",
		"--keep-yearly", "0",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		`"dry_run": true`,
		`"policy_name": "gfs"`,
		`"deployment": "db1"`,
		`"action": "keep"`,
		`"action": "delete"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	// Tombstone files MUST NOT exist after a dry-run.
	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	for _, ts := range stamps {
		id := "db1.full." + ts.UTC().Format("20060102T150405Z")
		if _, err := sp.Stat(context.Background(), backup.TombstonePath("db1", id)); err == nil {
			t.Errorf("tombstone unexpectedly present after dry-run for %s", id)
		}
	}
}

// A held backup MUST be excluded from rotate --apply's tombstone
// sweep. The dry-run report's `held` count tells the operator
// what would have been deleted but wasn't, and the `deleted` count
// reflects the post-filter total — so capacity reporting and SLO
// checks see the truth.
func TestRotate_RespectsHold(t *testing.T) {
	repoURL := initRepoForTest(t)

	// Plant 2 manifests: yesterday + today.
	now := time.Now().UTC()
	stamps := []time.Time{now.Add(-24 * time.Hour), now}
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
				Schema:           backup.Schema,
				BackupID:         id,
				Deployment:       "db1",
				Type:             backup.BackupTypeFull,
				PGVersion:        170,
				SystemIdentifier: "7388123",
				StartLSN:         "0/0",
				StopLSN:          "0/0",
				Timeline:         1,
				StartedAt:        ts.Add(-time.Minute),
				StoppedAt:        ts,
				BackupLabel:      "START WAL LOCATION: 0/0\n",
				Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
				Files:            []backup.FileEntry{},
			}
			if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
				t.Fatalf("commit: %v", err)
			}
		}
		sp.Close()
	}
	yesterdayID := "db1.full." + stamps[0].Format("20060102T150405Z")

	// Place a hold on yesterday. With keep-daily=1, GFS would
	// normally tombstone it; the hold must override that.
	if _, _, exit := runCmd(t,
		"hold", "add", "db1", yesterdayID,
		"--repo", repoURL,
		"--reason", "litigation hold X-100",
		"--output", "json",
	); exit != 0 {
		t.Fatalf("hold add: exit %d", exit)
	}

	out, _, exit := runCmd(t,
		"rotate", "db1",
		"--repo", repoURL,
		"--policy", "gfs",
		"--keep-daily", "1",
		"--keep-weekly", "0",
		"--keep-monthly", "0",
		"--keep-yearly", "0",
		"--apply",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("rotate --apply: exit %d, out:\n%s", exit, out)
	}
	for _, want := range []string{
		`"held": 1`,
		yesterdayID,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in rotate output:\n%s", want, out)
		}
	}
	// Verify on disk: yesterday's tombstone MUST NOT exist.
	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	if _, err := sp.Stat(context.Background(), backup.TombstonePath("db1", yesterdayID)); err == nil {
		t.Errorf("held backup %s was tombstoned despite hold", yesterdayID)
	}
}

func TestRotate_ApplyCreatesTombstones(t *testing.T) {
	repoURL := initRepoForTest(t)

	// Seed 3 daily backups; with --keep-daily 1 we expect 2 deletions.
	now := time.Now().UTC()
	stamps := []time.Time{
		now.Add(-1 * 24 * time.Hour),
		now.Add(-2 * 24 * time.Hour),
		now.Add(-3 * 24 * time.Hour),
	}
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
				Schema:           backup.Schema,
				BackupID:         id,
				Deployment:       "db1",
				Type:             backup.BackupTypeFull,
				PGVersion:        170,
				SystemIdentifier: "7388123",
				StartLSN:         "0/0",
				StopLSN:          "0/0",
				Timeline:         1,
				StartedAt:        ts.Add(-time.Minute),
				StoppedAt:        ts,
				BackupLabel:      "START WAL LOCATION: 0/0\n",
				Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
				Files:            []backup.FileEntry{},
			}
			if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
				t.Fatalf("commit: %v", err)
			}
		}
		_ = rand.Reader
		sp.Close()
	}

	// Apply. Expect tombstones for the two older backups.
	out, _, exit := runCmd(t,
		"rotate", "db1",
		"--repo", repoURL,
		"--policy", "gfs",
		"--keep-daily", "1",
		"--keep-weekly", "0",
		"--keep-monthly", "0",
		"--keep-yearly", "0",
		"--apply",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d\n%s", exit, out)
	}
	if !strings.Contains(out, `"applied": 2`) {
		t.Errorf("expected applied:2; got:\n%s", out)
	}

	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	// The newest stays alive, the two older are tombstoned.
	for i, ts := range stamps {
		id := "db1.full." + ts.UTC().Format("20060102T150405Z")
		_, statErr := sp.Stat(context.Background(), backup.TombstonePath("db1", id))
		if i == 0 && statErr == nil {
			t.Errorf("newest backup unexpectedly tombstoned: %s", id)
		}
		if i > 0 && statErr != nil {
			t.Errorf("expected tombstone for %s: %v", id, statErr)
		}
	}
}

func TestRotate_NoSuchDeployment(t *testing.T) {
	repoURL := initRepoForTest(t)
	_, _, exit := runCmd(t,
		"rotate", "ghost",
		"--repo", repoURL,
		"--output", "json",
	)
	if exit != 6 {
		t.Errorf("exit = %d, want 6 (notfound.deployment)", exit)
	}
}

func TestRotate_RequiresRepo(t *testing.T) {
	_, _, exit := runCmd(t, "rotate", "db1", "--output", "json")
	if exit != 2 {
		t.Errorf("exit = %d, want 2 (ExitMisuse)", exit)
	}
}

func TestRotate_UnknownPolicy(t *testing.T) {
	repoURL := initRepoForTest(t)
	_, _, exit := runCmd(t,
		"rotate",
		"--repo", repoURL,
		"--policy", "fortnight",
		"--output", "json",
	)
	if exit != 2 {
		t.Errorf("exit = %d, want 2 (ExitMisuse)", exit)
	}
}
