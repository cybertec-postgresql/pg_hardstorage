package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// TestDSALocate_UnknownTenantRefused pins that a mistyped --tenant is
// refused instead of producing a SIGNED report enumerating zero
// affected backups -- under Article 17 that is an instruction to shred
// nothing, filed as evidence the request was handled. Same reasoning
// as requireDeploymentExists for --deployment.
func TestDSALocate_UnknownTenantRefused(t *testing.T) {
	w := newReadWorld(t)
	commitTenantManifest(t, w, "db1", "tenant-a", "kms://acme/a", 1)

	_, errb, exit := runCLI(t, "dsa", "locate",
		"--repo", w.repoURL, "--subject-id", "u", "--tenant", "tenant-typo", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Fatalf("exit = %d, want ExitMisuse\n%s", exit, errb)
	}
	if !strings.Contains(errb, "usage.unknown_tenant") {
		t.Errorf("want usage.unknown_tenant:\n%s", errb)
	}

	// An operator asserting "this tenant holds nothing" can still get a
	// signed zero report, explicitly.
	stdout, errb, exit := runCLI(t, "dsa", "locate",
		"--repo", w.repoURL, "--subject-id", "u", "--tenant", "tenant-typo",
		"--allow-unknown-tenant", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("--allow-unknown-tenant: exit = %d\n%s%s", exit, stdout, errb)
	}
}

// TestDSALocate_UnreadableManifestsExitNonZero pins that a report with
// unreadable manifests -- a scan that may have missed the subject's
// data -- still renders (and is signed with the unreadable count) but
// exits non-zero, so automation cannot file it as complete.
func TestDSALocate_UnreadableManifestsExitNonZero(t *testing.T) {
	w := newReadWorld(t)
	commitTenantManifest(t, w, "db1", "tenant-a", "kms://acme/a", 1)
	commitTenantManifest(t, w, "db1", "tenant-a", "kms://acme/a", 2)
	var key string
	for info, err := range w.sp.List(context.Background(), "manifests/db1/backups/") {
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(info.Key, "/manifest.json") {
			key = info.Key
		}
	}
	body := []byte(`{"schema":"broken"}`)
	if _, err := w.sp.Put(context.Background(), key, bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}

	stdout, errb, exit := runCLI(t, "dsa", "locate",
		"--repo", w.repoURL, "--subject-id", "u", "--tenant", "tenant-a", "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("exit = %d, want ExitVerifyFailed\n%s%s", exit, stdout, errb)
	}
	if !strings.Contains(errb, "verify.dsa_incomplete") {
		t.Errorf("want verify.dsa_incomplete:\n%s", errb)
	}
	if !strings.Contains(stdout, `"manifests_unreadable": 1`) {
		t.Errorf("report body must still render with the unreadable count:\n%s", stdout)
	}
}
