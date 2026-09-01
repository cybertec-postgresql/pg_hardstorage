package cli_test

// deployment_filter_test.go — a --deployment that matches nothing must
// not produce a clean verdict.
//
// `kms verify` and `repo replicate verify` both scope their walk by
// building a key prefix out of the flag:
//
//	manifests/<deployment>/backups/
//
// An unknown name lists nothing, so every counter lands on zero,
// nothing is classified as broken, and the command exits 0. `kms
// verify` printed the operator's own typo back as the scope it had
// checked ("Scope: deployment \"db-typo\"") and `repo replicate verify`
// answered `consistent` — "the replica has everything" — having
// compared nothing. A compliance job pointed at a renamed or retired
// deployment reports green forever, which is the worst possible failure
// for a scheduled check: it is indistinguishable from working.
//
// Both are verdict-producing commands with their own non-zero exit
// codes, and that is what separates them from the listing commands
// (`audit search`, `forecast`, `compliance`), where an empty filtered
// view is a legitimate answer and no pass/fail is claimed.
//
// The check is on the NAME, not on the resulting count: zero manifests
// is also the right answer for a deployment that exists and has had
// every backup tombstoned, so refusing on an empty walk would break a
// legitimate run.

import (
	"crypto/rand"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

func TestKmsVerify_UnknownDeploymentIsRefusedNotPassed(t *testing.T) {
	w := newReadWorld(t)
	var kek [encryption.KeyLen]byte
	if _, err := rand.Read(kek[:]); err != nil {
		t.Fatal(err)
	}
	installLocalKEK(t, kek)
	commitEncryptedBackup(t, w, "db1", "ok", 1, kek, keystore.KEKRefLocal, []byte("plaintext-A"))

	stdout, errb, exit := runCLI(t, "kms", "verify",
		"--repo", w.repoURL, "--deployment", "db-typo", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("a --deployment that matches nothing exited 0:\n%s", stdout)
	}
	if !strings.Contains(errb, "usage.unknown_deployment") {
		t.Errorf("expected usage.unknown_deployment, got:\n%s", errb)
	}
	// The message has to be actionable: name the typo AND the real one.
	for _, want := range []string{"db-typo", "db1"} {
		if !strings.Contains(errb, want) {
			t.Errorf("error does not mention %q — an operator cannot spot the typo:\n%s", want, errb)
		}
	}
}

// The guard must not break the filter it is protecting: a real
// deployment still runs and still reports its own scope.
func TestKmsVerify_KnownDeploymentStillRuns(t *testing.T) {
	w := newReadWorld(t)
	var kek [encryption.KeyLen]byte
	if _, err := rand.Read(kek[:]); err != nil {
		t.Fatal(err)
	}
	installLocalKEK(t, kek)
	commitEncryptedBackup(t, w, "db1", "ok", 1, kek, keystore.KEKRefLocal, []byte("plaintext-A"))

	stdout, errb, exit := runCLI(t, "kms", "verify",
		"--repo", w.repoURL, "--deployment", "db1", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d for a real deployment\n%s\n%s", exit, stdout, errb)
	}
	var view kmsVerifyView
	bodyOf(t, stdout, &view)
	if view.DeploymentFilter != "db1" {
		t.Errorf("deployment_filter = %q, want db1", view.DeploymentFilter)
	}
	if view.Considered == 0 {
		t.Error("considered = 0 for a deployment that has a backup")
	}
}

// No filter at all must stay unaffected, including on an empty repo —
// "walk everything, find nothing" is a legitimate clean result.
func TestKmsVerify_NoFilterOnEmptyRepoStillExitsOK(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "kms", "verify", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("unfiltered run on an empty repo exit = %d\n%s", exit, stdout)
	}
}

func TestRepoReplicateVerify_UnknownDeploymentIsRefusedNotConsistent(t *testing.T) {
	w := newDualReadWorld(t)
	commitVerifiableBackup(t, w.readWorld, "db1", 0, []byte("a"))
	w.replicate(t)

	stdout, errb, exit := runCLI(t, "repo", "replicate", "verify",
		"--from", w.repoURL, "--to", w.dstURL,
		"--deployment", "db-typo", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("an unknown --deployment reported a consistent replica and exited 0:\n%s", stdout)
	}
	if !strings.Contains(errb, "usage.unknown_deployment") {
		t.Errorf("expected usage.unknown_deployment, got:\n%s", errb)
	}
}

// And the real deployment still verifies clean, so the guard has not
// simply broken the flag.
func TestRepoReplicateVerify_KnownDeploymentStillRuns(t *testing.T) {
	w := newDualReadWorld(t)
	commitVerifiableBackup(t, w.readWorld, "db1", 0, []byte("a"))
	w.replicate(t)

	stdout, errb, exit := runCLI(t, "repo", "replicate", "verify",
		"--from", w.repoURL, "--to", w.dstURL,
		"--deployment", "db1", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d for a real deployment\n%s\n%s", exit, stdout, errb)
	}
	var view replicateVerifyView
	bodyOf(t, stdout, &view)
	if view.Verdict != "consistent" {
		t.Errorf("Verdict = %q, want consistent", view.Verdict)
	}
}

// `integrity run` is the sharpest case of the same trap, because its
// result does not just print — it is SIGNED with the operator's key and
// persisted under integrity/runs/<id>.json, which the command's own
// help describes as the artefact "an auditor can prove the repo was
// intact at any historical attest time" with.
//
// A --deployment that names nothing walked nothing: Manifests.Total 0,
// no failures, StatusOK, exit 0 — and that clean attestation was then
// signed and durably stored. A typo would mint evidence of an integrity
// check that examined zero manifests.
func TestIntegrityRun_UnknownDeploymentIsRefusedNotAttested(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("a"))

	stdout, errb, exit := runCLI(t, "integrity", "run",
		"--repo", w.repoURL, "--deployment", "db-typo",
		"--strategy", "manifests-only", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("an unknown --deployment produced a clean integrity attestation:\n%s", stdout)
	}
	if !strings.Contains(errb, "usage.unknown_deployment") {
		t.Errorf("expected usage.unknown_deployment, got:\n%s", errb)
	}
}

// And the real deployment still attests, or the guard has broken the
// command it protects.
func TestIntegrityRun_KnownDeploymentStillAttests(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("a"))

	stdout, errb, exit := runCLI(t, "integrity", "run",
		"--repo", w.repoURL, "--deployment", "db1",
		"--strategy", "manifests-only", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d for a real deployment\n%s\n%s", exit, stdout, errb)
	}
}
