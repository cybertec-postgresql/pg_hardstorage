package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

func TestRepoCheck_RequiresURL(t *testing.T) {
	_ = newReadWorld(t)
	_, _, exit := runCLI(t, "repo", "check", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
}

func TestRepoCheck_HealthyEmpty(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "repo", "check", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		`"healthy": true`,
		`"missing_chunks": 0`,
		`"live_manifests": 0`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
}

func TestRepoCheck_HealthyWithBackup(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("real chunk"))
	commitVerifiableBackup(t, w, "db2", 0, []byte("another chunk"))

	stdout, _, exit := runCLI(t, "repo", "check", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d, out:\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"healthy": true`,
		`"missing_chunks": 0`,
		`"live_manifests": 2`,
		`"signature_failures": 0`,
		`"name": "db1"`,
		`"name": "db2"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
}

func TestRepoCheck_MissingChunk_FailsVerify(t *testing.T) {
	// Commit a backup that references a chunk; then delete the chunk
	// out from under the manifest. repo check must report the missing
	// reference and exit ExitVerifyFailed.
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("doomed"))

	cas := casdefault.New(w.sp)
	if err := cas.DeleteChunk(context.Background(), repo.HashOf([]byte("doomed"))); err != nil {
		t.Fatalf("delete chunk: %v", err)
	}

	_, errb, exit := runCLI(t, "repo", "check", w.repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("exit = %d, want ExitVerifyFailed(%d)\nstderr: %s",
			exit, output.ExitVerifyFailed, errb)
	}
	if !strings.Contains(errb, "verify.missing_chunks") {
		t.Errorf("expected verify.missing_chunks code (so the exit code routes to ExitVerifyFailed):\n%s", errb)
	}
	// The suggestion must point operators at the right repair command.
	if !strings.Contains(errb, "repair chunks --missing") {
		t.Errorf("suggestion should mention `repair chunks --missing`:\n%s", errb)
	}
}

// TestRepoCheck_SignatureFailure_FailsVerify pins the fix for
// the silent-corruption bug surfaced by
// L8_repo_check_detects_manifest_corruption: when a manifest's
// bytes are mutated post-commit, the manifest's Ed25519
// signature no longer verifies, and `repo check` must surface
// that with a non-zero exit (ExitVerifyFailed) AND
// `healthy: false` in the body.  Pre-fix, the same input
// produced `signature_failures: 1` in the body but
// `healthy: true` and exit 0 — operators running `repo check`
// in cron would never notice corruption.
func TestRepoCheck_SignatureFailure_FailsVerify(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("ok-body"))

	// Mutate one byte deep inside the manifest JSON.  Either
	// the signature check fails (canonical-bytes hash mismatch)
	// or the JSON fails to parse — both flow into
	// SignatureFailures from the verifier's perspective.
	manifestPath := filepath.Join(strings.TrimPrefix(w.repoURL, "file://"),
		"manifests", "db1", "backups", id, "manifest.json")
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest %s: %v", manifestPath, err)
	}
	if len(body) < 256 {
		t.Fatalf("manifest unexpectedly small (%d bytes); test offset assumption broken", len(body))
	}
	body[128] ^= 0x01
	if err := os.WriteFile(manifestPath, body, 0o644); err != nil {
		t.Fatalf("write corrupted manifest: %v", err)
	}

	_, errb, exit := runCLI(t, "repo", "check", w.repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("exit = %d, want ExitVerifyFailed(%d)\nstderr: %s",
			exit, output.ExitVerifyFailed, errb)
	}
	if !strings.Contains(errb, "verify.signature_failures") {
		t.Errorf("expected verify.signature_failures error code:\n%s", errb)
	}
}

func TestRepoCheck_AcceptsRepoFlag(t *testing.T) {
	w := newReadWorld(t)
	_, _, exit := runCLI(t,
		"repo", "check", "--repo", w.repoURL, "-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Errorf("--repo flag form should work; exit = %d", exit)
	}
}

func TestRepoCheck_RejectsConflictingRepoSources(t *testing.T) {
	_ = newReadWorld(t)
	_, _, exit := runCLI(t,
		"repo", "check", "file:///foo",
		"--repo", "file:///bar",
		"-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("conflicting positional + --repo should exit ExitMisuse(2); got %d", exit)
	}
}
