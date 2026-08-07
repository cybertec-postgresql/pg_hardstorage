package cli_test

// wal_push_foreign_cluster_test.go — `wal push` must refuse WAL from a
// different cluster than the deployment already holds.
//
// `wal stream` has guarded this since the pg_upgrade work
// (guardSystemIdentifier). `wal push` — the archive_command path, the
// one PG invokes unattended thousands of times a day — did not.
//
// The reason it looked covered is verifyExistingManifest, which raises
// splitbrain.system_identifier_mismatch when two clusters archive the
// SAME segment number. That is a duplicate check, not a continuity
// check. A foreign cluster whose segment numbers happen not to collide
// — the normal case after a pg_upgrade, which starts at a higher LSN —
// archived into the deployment unopposed. Measured before the fix:
// exit 0.
//
// The damage is not confined to a cluttered archive. Every resume and
// gap computation reads the archive frontier through
// inventory.HighestArchivedLSN, which returns the highest segment's end
// LSN without regard to which cluster wrote it. A foreign segment at a
// higher number drags that frontier forward, and `wal stream` then
// resumes past WAL the real cluster has not archived yet. From the
// streamer's side the frontier looks entirely ordinary, so the loss is
// silent.
//
// How a foreign cluster comes to share a deployment is not exotic: a
// pg_upgrade changes the system identifier while archive_command stays
// exactly as it was.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
)

const (
	clusterA = "7000000000000000001"
	clusterB = "9999999999999999999"
)

// pushSegment archives one synthetic segment under the given identity.
func pushSegment(t *testing.T, repoURL, segName, sysID string, extra ...string) (int, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), segName)
	body := make([]byte, walsink.SegmentSize)
	for i := range body {
		body[i] = byte((i*13 + 7) % 256)
	}
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"wal", "push", "db1", p,
		"--repo", repoURL, "--system-identifier", sysID, "-o", "json"}, extra...)
	_, errb, exit := runCLI(t, args...)
	return exit, errb
}

// TestWalPush_RefusesAForeignCluster is the regression test.
//
// Segment numbers deliberately do NOT collide (5 then 9), because a
// collision is the one case that was already caught. This is the case
// that was not.
func TestWalPush_RefusesAForeignCluster(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", t.TempDir())
	repoURL := initRepoForTest(t)

	if exit, errb := pushSegment(t, repoURL, "000000010000000000000005", clusterA); exit != int(output.ExitOK) {
		t.Fatalf("the first cluster could not establish the deployment: exit=%d\n%s", exit, errb)
	}

	exit, errb := pushSegment(t, repoURL, "000000010000000000000009", clusterB)
	if exit == int(output.ExitOK) {
		t.Fatalf("a foreign cluster (system_identifier %s) archived into a deployment "+
			"established by %s, and `wal push` returned 0.\n\n"+
			"Segment numbers 5 and 9 do not collide, so verifyExistingManifest never "+
			"compared them — it only fires on the same segment number. Two clusters now "+
			"share one lineage, and inventory.HighestArchivedLSN will report the foreign "+
			"segment as the archive frontier, so `wal stream` resumes past WAL the real "+
			"cluster has not archived.", clusterB, clusterA)
	}
	if !strings.Contains(errb, "system_identifier_changed") {
		t.Errorf("refused with the wrong code; want preflight.system_identifier_changed:\n%s", errb)
	}
	// The remediation matters as much as the refusal: after a
	// pg_upgrade the operator's correct move is a fresh deployment,
	// which keeps the old lineage restorable.
	for _, want := range []string{"FRESH deployment", "allow-system-identifier-change"} {
		if !strings.Contains(errb, want) {
			t.Errorf("the refusal does not mention %q — an operator mid-upgrade needs to be "+
				"told what to do, not just what was refused:\n%s", want, errb)
		}
	}
}

// TestWalPush_AllowsAForeignClusterWhenAsked: a pg_upgrade IS a
// deliberate continuation for some operators, and the escape hatch has
// to work or they will find a worse one.
func TestWalPush_AllowsAForeignClusterWhenAsked(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", t.TempDir())
	repoURL := initRepoForTest(t)

	if exit, errb := pushSegment(t, repoURL, "000000010000000000000005", clusterA); exit != int(output.ExitOK) {
		t.Fatalf("first push: exit=%d\n%s", exit, errb)
	}
	if exit, errb := pushSegment(t, repoURL, "000000010000000000000009", clusterB,
		"--allow-system-identifier-change"); exit != int(output.ExitOK) {
		t.Fatalf("--allow-system-identifier-change did not permit the push: exit=%d\n%s", exit, errb)
	}
}

// TestWalPush_SameClusterIsUnaffected pins the ordinary path. This
// guard runs on every archive_command invocation, so a false positive
// would stop WAL archiving for a healthy cluster — strictly worse than
// the bug being fixed.
func TestWalPush_SameClusterIsUnaffected(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", t.TempDir())
	repoURL := initRepoForTest(t)

	for _, seg := range []string{
		"000000010000000000000005",
		"000000010000000000000006",
		"000000010000000000000007",
	} {
		if exit, errb := pushSegment(t, repoURL, seg, clusterA); exit != int(output.ExitOK) {
			t.Fatalf("push %s from the SAME cluster was refused: exit=%d\n%s", seg, exit, errb)
		}
	}
}

// TestWalPush_FirstPushEstablishesTheDeployment: an empty deployment
// has no recorded identity, so the first push must succeed whatever the
// identifier is. Refusing here would make the guard unbootstrappable.
func TestWalPush_FirstPushEstablishesTheDeployment(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", t.TempDir())
	repoURL := initRepoForTest(t)

	if exit, errb := pushSegment(t, repoURL, "000000010000000000000001", clusterB); exit != int(output.ExitOK) {
		t.Fatalf("the first push into an empty deployment was refused: exit=%d\n%s", exit, errb)
	}
}
