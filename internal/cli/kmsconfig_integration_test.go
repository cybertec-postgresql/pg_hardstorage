// Build-tagged integration tests: verify --full and partial dump exercise a
// Docker sandbox / sandbox PG, so the --kms-config-reaches-the-provider proof
// for those two paths lives here. The provider builder is consulted during
// the (pure-Go) restore phase that precedes the sandbox/PG step.
//
//go:build integration

package cli_test

import (
	"path/filepath"
	"testing"

	psandbox "github.com/cybertec-postgresql/pg_hardstorage/internal/partial/sandbox"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

// TestVerifyFull_CloudKMS_PassesKMSConfigToProvider: `verify --full
// --kms-config ...` reaches the cloud KMS provider builder. verify --full
// restores (where the provider is consulted) before its Docker sandbox step;
// we assert only that the config reached the provider — the sandbox verdict
// for the minimal backup is irrelevant.
func TestVerifyFull_CloudKMS_PassesKMSConfigToProvider(t *testing.T) {
	w := newReadWorld(t)
	dek, err := encryption.GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	id := commitCloudEncryptedBackup(t, w, "db1", "kmscfg-vfull://key", "data/file", dek, []byte("17\n"))

	getCfg := recordingKMSProvider(t, "kmscfg-vfull")

	stdout, stderr, exit := runCLI(t, "verify", "db1", id, "--full",
		"--repo", w.repoURL, "--kms-config", "region=eu-north-1", "-o", "json")
	cfg := getCfg()
	if cfg == nil {
		t.Fatalf("provider builder never called (verify --full exit=%d)\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if cfg["region"] != "eu-north-1" {
		t.Errorf("provider cfg region = %v, want eu-north-1 (the --kms-config flag)", cfg["region"])
	}
}

// TestPartialDump_CloudKMS_PassesKMSConfigToProvider: `partial dump
// --kms-config ...` reaches the cloud KMS provider builder. partial dump does
// a full restore (where the provider is consulted) before pg_dump; it
// pre-flights pg_ctl + pg_dump BEFORE that restore, so we skip if absent.
func TestPartialDump_CloudKMS_PassesKMSConfigToProvider(t *testing.T) {
	if _, _, err := psandbox.DiscoverPGTools(); err != nil {
		t.Skip("partial dump pre-flights pg_ctl + pg_dump before the cloud-KMS restore; not on PATH")
	}
	w := newReadWorld(t)
	dek, err := encryption.GenerateDEK()
	if err != nil {
		t.Fatal(err)
	}
	id := commitCloudEncryptedBackup(t, w, "db1", "kmscfg-pdump://key", "data/file", dek, []byte("17\n"))

	getCfg := recordingKMSProvider(t, "kmscfg-pdump")

	stdout, stderr, exit := runCLI(t, "partial", "dump", "db1",
		"--repo", w.repoURL, "--backup", id, "--tables", "public.users",
		"--sql-file", filepath.Join(t.TempDir(), "out.sql"),
		"--kms-config", "region=ca-central-1", "-o", "json")
	cfg := getCfg()
	if cfg == nil {
		t.Fatalf("provider builder never called (partial dump exit=%d)\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if cfg["region"] != "ca-central-1" {
		t.Errorf("provider cfg region = %v, want ca-central-1 (the --kms-config flag)", cfg["region"])
	}
}
