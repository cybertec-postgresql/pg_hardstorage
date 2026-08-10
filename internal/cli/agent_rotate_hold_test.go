package cli

// agent_rotate_hold_test.go — the SCHEDULED (agent) rotation path must
// survive a hold on a non-leaf chain member, exactly as the interactive
// `rotate` command does.
//
// TestRotate_HoldOnIncrementalDoesNotWedgeRetention pins the fix for
// the INTERACTIVE path (rotate.go's filterHeld protects a held backup's
// ancestors before the batch runs). The agent's scheduled rotation is a
// SEPARATE code path (buildRotateTask -> store.SoftDelete in a loop)
// that never gained the same protection: it skips a held manifest
// (ErrManifestHeld) but treats the parent's ChainHasLiveDescendantsError
// — the parent kept alive by that very held child — as run-fatal and
// aborts. Every scheduled rotation then errors for the life of the
// hold, and any deletable backup ordered after the held chain's anchor
// is never pruned: unbounded repo/WAL growth on the unattended path,
// which is the one nobody is watching.
//
// The agent must skip that refusal too and keep pruning the rest.

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func TestAgentRotate_HoldOnIncrementalDoesNotWedgeScheduledRetention(t *testing.T) {
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	store := backup.NewManifestStore(sp)

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	now := time.Now().UTC()
	plant := func(id, parent string, bt backup.BackupType, ts time.Time) {
		t.Helper()
		m := &backup.Manifest{
			Schema: backup.Schema, BackupID: id, Deployment: "db1", Tenant: "default",
			Type: bt, ParentBackupID: parent, PGVersion: 17,
			SystemIdentifier: "7000000000000000001",
			StartLSN:         "0/0", StopLSN: "0/0", Timeline: 1,
			StartedAt: ts.Add(-time.Minute), StoppedAt: ts,
			BackupLabel: "START WAL LOCATION: 0/0\n",
			Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
			Files:       []backup.FileEntry{},
		}
		if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatalf("commit %s: %v", id, err)
		}
	}

	// Old chain F<-I1 (both past the retention window), an UNRELATED old
	// full Z that is OLDER than F (so it sorts AFTER F in the newest-
	// first delete order — it only gets pruned if the loop survives the
	// abort at F), and a fresh full R the policy keeps.
	plant("F", "", backup.BackupTypeFull, now.Add(-40*24*time.Hour))
	plant("I1", "F", backup.BackupTypeIncremental, now.Add(-39*24*time.Hour))
	plant("Z", "", backup.BackupTypeFull, now.Add(-50*24*time.Hour))
	plant("R", "", backup.BackupTypeFull, now.Add(-1*time.Hour))

	// Hold ONLY the incremental — the leaf that keeps F alive.
	if err := store.PutHold(context.Background(), "db1", "I1", "ops", "litigation X-9"); err != nil {
		t.Fatalf("PutHold: %v", err)
	}

	dep := config.DeploymentConfig{
		Repo:      repoURL,
		Retention: config.RetentionConfig{Policy: "simple", KeepFor: "168h"},
		Schedule:  config.DeploymentSchedule{Rotate: config.ScheduleSpec{Every: "24h"}},
	}
	task, err := buildRotateTask("db1", dep, verifier)
	if err != nil {
		t.Fatalf("buildRotateTask: %v", err)
	}

	if err := task.Run(context.Background()); err != nil {
		t.Fatalf("scheduled rotation aborted on a held chain's anchor: %v\n\n"+
			"the agent must skip a parent kept live by its held child and keep pruning, "+
			"or one hold wedges every nightly rotation and the repo grows unbounded", err)
	}

	dead := func(id string) bool {
		d, derr := store.IsTombstoned(context.Background(), "db1", id)
		if derr != nil {
			t.Fatal(derr)
		}
		return d
	}
	// Z is older than F, so it is the tail of the delete order — it is
	// pruned only if the loop did not abort at F.
	if !dead("Z") {
		t.Error("unrelated deletable full Z (ordered after the held chain's anchor) was not " +
			"tombstoned — the scheduled rotation aborted early and left it behind")
	}
	if dead("F") {
		t.Error("F tombstoned despite being the held chain's live anchor")
	}
	if dead("I1") {
		t.Error("held incremental I1 tombstoned by scheduled rotation")
	}
	if dead("R") {
		t.Error("fresh keeper R tombstoned")
	}
}
