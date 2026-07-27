package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// A hold on an INCREMENTAL used to wedge retention for the whole
// deployment: filterHeld removed the held child from the delete
// batch but left its parent IN, SoftDeleteBatch's chain guard then
// refused the entire batch ("live descendant not in batch"), and for
// the lifetime of the hold every scheduled `rotate --apply` deleted
// NOTHING — unbounded repo/WAL growth behind a dry-run report that
// claimed a normal sweep. The filter must be chain-aware: protect
// the held backup's ancestors and delete the rest.
func TestRotate_HoldOnIncrementalDoesNotWedgeRetention(t *testing.T) {
	repoURL := initRepoForTest(t)

	now := time.Now().UTC()
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

	plant := func(id, parent string, btype backup.BackupType, ts time.Time) {
		t.Helper()
		m := &backup.Manifest{
			Schema:           backup.Schema,
			BackupID:         id,
			Deployment:       "db1",
			Type:             btype,
			ParentBackupID:   parent,
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
			t.Fatalf("commit %s: %v", id, err)
		}
	}

	// Old chain F←I1 (both policy-deletable), an unrelated old full G
	// (deletable), and a fresh full R the policy keeps.
	plant("F", "", backup.BackupTypeFull, now.Add(-40*24*time.Hour))
	plant("I1", "F", backup.BackupTypeIncremental, now.Add(-39*24*time.Hour))
	plant("G", "", backup.BackupTypeFull, now.Add(-35*24*time.Hour))
	plant("R", "", backup.BackupTypeFull, now.Add(-1*time.Hour))

	// Hold ONLY the incremental.
	if _, _, exit := runCmd(t, "hold", "add", "db1", "I1",
		"--repo", repoURL, "--reason", "litigation X-9"); exit != 0 {
		t.Fatalf("hold add: exit %d", exit)
	}

	out, stderr, exit := runCmd(t,
		"rotate", "db1", "--repo", repoURL,
		"--policy", "simple", "--keep-for", "168h",
		"--apply", "--output", "json",
	)
	if exit != 0 {
		t.Fatalf("rotate --apply wedged by the held incremental (exit %d):\n%s%s", exit, out, stderr)
	}

	// G must be gone; F kept as chain anchor; I1 kept by the hold.
	dead := func(id string) bool {
		d, derr := store.IsTombstoned(context.Background(), "db1", id)
		if derr != nil {
			t.Fatal(derr)
		}
		return d
	}
	if !dead("G") {
		t.Error("unrelated deletable full G was not tombstoned — retention still wedged")
	}
	if dead("F") {
		t.Error("F tombstoned despite being the held chain's anchor")
	}
	if dead("I1") {
		t.Error("held incremental I1 tombstoned")
	}
	if dead("R") {
		t.Error("fresh keeper R tombstoned")
	}
	if !strings.Contains(out, "held_chain_anchor_ids") || !strings.Contains(out, `"F"`) {
		t.Errorf("report should surface F as held_chain_anchor_ids:\n%s", out)
	}
}
