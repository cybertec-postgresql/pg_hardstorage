package cli_test

// repo_check_orphan_replica_test.go — a repository that had silently
// lost backups reported healthy.
//
// Every commit writes the primary manifest and then a redundancy copy
// at manifests/_replicas/<id>.manifest.json, for the reason the store's
// own doc gives: "if the primary is lost (a single misdirected
// `aws s3 rm`, say), the replica still has the bytes".
// `repair manifest <deployment> <backup-id>` restores the primary from
// it, re-verifying the signature first.
//
// That recovery needs a backup ID — and a backup whose primary is gone
// appears in no listing at all. ManifestStore.List walks
// manifests/<dep>/backups/, so `list`, `status` and `restore` show
// nothing, and `repo check`'s LiveManifests count simply got smaller.
// Nothing compared the two prefixes. The redundancy was therefore
// unusable in the exact scenario it exists for: the bytes survived, and
// no command would tell the operator which backups to recover.
//
// Worse for a health command: `repo check` reported healthy=true and
// exit 0 over a repository that had lost backups, with the evidence
// sitting one prefix away.

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

type repoCheckView struct {
	Healthy          bool     `json:"healthy"`
	LiveManifests    int      `json:"live_manifests"`
	OrphanedReplicas []string `json:"orphaned_replicas"`
}

func TestRepoCheck_OrphanedReplicaIsReported(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	// Healthy to begin with.
	stdout, _, exit := runCLI(t, "repo", "check", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("healthy repo: exit=%d\n%s", exit, stdout)
	}

	// The primary manifest is deleted; the redundancy copy survives —
	// the misdirected-rm scenario the replica exists for.
	primary := "manifests/db1/backups/" + id + "/manifest.json"
	if err := w.sp.Delete(context.Background(), primary); err != nil {
		t.Fatal(err)
	}
	if _, err := w.sp.Stat(context.Background(), backup.ReplicaPath(id)); err != nil {
		t.Fatalf("fixture: the redundancy copy is missing, so this test proves nothing: %v", err)
	}

	stdout, errb, exit := runCLI(t, "repo", "check", "--repo", w.repoURL, "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("repo check reported healthy after a backup's primary manifest was "+
			"deleted.\n\nThe backup is now invisible to list/status/restore, its redundancy "+
			"copy is sitting in manifests/_replicas/, and nothing tells the operator which "+
			"ID to hand `repair manifest`.\n%s", stdout)
	}
	if !strings.Contains(errb, "verify.orphaned_replicas") {
		t.Errorf("expected verify.orphaned_replicas, got:\n%s", errb)
	}
	// On a finding this command returns the structured error without
	// also emitting the body — the same shape its missing-chunks and
	// signature-failure branches have — so the ID has to be in the
	// message. Discovering that ID is the entire point of the check.
	if !strings.Contains(errb, id) {
		t.Errorf("the finding does not name the recoverable backup ID, which is the one "+
			"thing the operator cannot obtain any other way:\n%s", errb)
	}
	if !strings.Contains(errb, "repair manifest") {
		t.Errorf("the finding does not point at the recovery command:\n%s", errb)
	}
}

// A tombstoned backup keeps its primary manifest — the tombstone is a
// separate marker — so its replica is NOT orphaned. Reporting it would
// make every soft-deleted backup look like data loss.
func TestRepoCheck_TombstonedBackupIsNotAnOrphanedReplica(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	if err := w.store.SoftDelete(context.Background(), "db1", id, "manual", "test"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	stdout, errb, exit := runCLI(t, "repo", "check", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("a soft-deleted backup was reported as an orphaned replica:\n%s\n%s",
			stdout, errb)
	}
	var view repoCheckView
	bodyOf(t, stdout, &view)
	if len(view.OrphanedReplicas) != 0 {
		t.Errorf("orphaned_replicas = %v for a tombstoned backup; its primary manifest is "+
			"intact and the tombstone is a separate marker", view.OrphanedReplicas)
	}
}

// A healthy repo must report none, or the finding fires everywhere and
// stops meaning anything.
func TestRepoCheck_HealthyRepoHasNoOrphanedReplicas(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("a"))
	commitVerifiableBackup(t, w, "db2", 1, []byte("b"))

	stdout, _, exit := runCLI(t, "repo", "check", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("healthy repo: exit=%d\n%s", exit, stdout)
	}
	var view repoCheckView
	bodyOf(t, stdout, &view)
	if len(view.OrphanedReplicas) != 0 {
		t.Errorf("orphaned_replicas = %v on a healthy repo", view.OrphanedReplicas)
	}
}

// With more than one orphan, every ID must reach the operator. The
// suggestion only carries the first as an example command, so the
// message itself has to list them — otherwise a repository that lost
// five backups discloses one and the operator recovers one.
func TestRepoCheck_AllOrphanedReplicaIDsAreListed(t *testing.T) {
	w := newReadWorld(t)
	ids := []string{
		commitVerifiableBackup(t, w, "db1", 0, []byte("a")),
		commitVerifiableBackup(t, w, "db1", 1, []byte("b")),
		commitVerifiableBackup(t, w, "db1", 2, []byte("c")),
	}
	for _, id := range ids {
		if err := w.sp.Delete(context.Background(),
			"manifests/db1/backups/"+id+"/manifest.json"); err != nil {
			t.Fatal(err)
		}
	}

	_, errb, exit := runCLI(t, "repo", "check", "--repo", w.repoURL, "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatal("three lost primaries reported healthy")
	}
	for _, id := range ids {
		if !strings.Contains(errb, id) {
			t.Errorf("backup %s is recoverable but its ID never reaches the operator; the\n"+
				"suggestion only carries the first as an example, so the message must list\n"+
				"them all:\n%s", id, errb)
		}
	}
}
