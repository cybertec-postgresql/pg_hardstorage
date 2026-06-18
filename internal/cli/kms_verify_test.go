package cli_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// commitEncryptedBackup plants an encrypted backup whose DEK is wrapped
// under kek with the given kek_ref. Mirrors the rotateWorld pattern from
// internal/backup tests but works against the CLI's readWorld fixture.
func commitEncryptedBackup(t *testing.T, w *readWorld, deployment, suffix string, idx int, kek [encryption.KeyLen]byte, kekRef string, body []byte) string {
	t.Helper()
	var dek [encryption.KeyLen]byte
	if _, err := rand.Read(dek[:]); err != nil {
		t.Fatal(err)
	}
	wrapped, err := encryption.Wrap(kek, dek)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := aesgcm.New(dek[:])
	if err != nil {
		t.Fatal(err)
	}
	cas := casdefault.NewEncrypted(w.sp, enc)
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatalf("put chunk: %v", err)
	}
	ts := time.Date(2026, 4, 30, 12, idx, 0, 0, time.UTC)
	id := deployment + ".enc." + suffix + "." + ts.Format("20060102T150405Z")
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        ts,
		StoppedAt:        ts.Add(30 * time.Second),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Encryption: &backup.EncryptionInfo{
			Scheme:          "aes-256-gcm",
			KEKRef:          kekRef,
			WrappedDEK:      base64.StdEncoding.EncodeToString(wrapped),
			EnvelopeVersion: 1,
		},
		Files: []backup.FileEntry{{
			Path: "data/" + id, Size: int64(len(body)), Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: int64(len(body))}},
		}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

// installLocalKEK writes raw bytes to <keyringDir>/kek.bin so the
// CLI's keystore.KEKResolver can resolve "local:default". Mode 0600.
func installLocalKEK(t *testing.T, kek [encryption.KeyLen]byte) {
	t.Helper()
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.Keyring.Value, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.Keyring.Value, keystore.KEKFileName)
	if err := os.WriteFile(path, kek[:], 0o600); err != nil {
		t.Fatal(err)
	}
}

// kmsVerifyView is the JSON shape we assert against. Mirrors
// kmsVerifyBody / VerifyEnvelopesResult.
type kmsVerifyView struct {
	Schema            string `json:"schema"`
	Considered        int    `json:"considered"`
	OK                int    `json:"ok"`
	Unencrypted       int    `json:"unencrypted"`
	KEKUnknown        int    `json:"kek_unknown"`
	WrappedDEKCorrupt int    `json:"wrapped_dek_corrupt"`
	UnwrapFailed      int    `json:"unwrap_failed"`
	UnknownScheme     int    `json:"unknown_scheme"`
	SignatureFailed   int    `json:"signature_failed"`
	Skipped           int    `json:"skipped"`
	DeploymentFilter  string `json:"deployment_filter"`
	KEKRefFilter      string `json:"kek_ref_filter"`
	Failures          []struct {
		Deployment string `json:"deployment"`
		BackupID   string `json:"backup_id"`
		KEKRef     string `json:"kek_ref"`
		Status     string `json:"status"`
		Reason     string `json:"reason"`
	} `json:"failures"`
}

// TestKmsVerify_RequiresRepo: --repo is mandatory.
func TestKmsVerify_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "kms", "verify", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

// TestKmsVerify_KekFileWithoutRef: --kek-file requires --kek-ref so we
// know which ref the bytes belong to.
func TestKmsVerify_KekFileWithoutRef(t *testing.T) {
	w := newReadWorld(t)
	tmp := filepath.Join(t.TempDir(), "kek.bin")
	if err := os.WriteFile(tmp, make([]byte, encryption.KeyLen), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errb, exit := runCLI(t, "kms", "verify",
		"--repo", w.repoURL, "--kek-file", tmp, "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestKmsVerify_EmptyRepo: a repo with no manifests reports
// considered=0 and exits cleanly.
func TestKmsVerify_EmptyRepo(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "kms", "verify",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view kmsVerifyView
	bodyOf(t, stdout, &view)
	if view.Considered != 0 || view.OK != 0 {
		t.Errorf("counts = %+v", view)
	}
}

// TestKmsVerify_HappyPath_LocalKEK: an encrypted backup wrapped under
// the local-keyring KEK with KEKRef="local:default" verifies cleanly.
func TestKmsVerify_HappyPath_LocalKEK(t *testing.T) {
	w := newReadWorld(t)
	var kek [encryption.KeyLen]byte
	if _, err := rand.Read(kek[:]); err != nil {
		t.Fatal(err)
	}
	installLocalKEK(t, kek)
	commitEncryptedBackup(t, w, "db1", "ok", 1, kek, keystore.KEKRefLocal, []byte("plaintext-A"))
	commitEncryptedBackup(t, w, "db1", "ok2", 2, kek, keystore.KEKRefLocal, []byte("plaintext-B"))

	stdout, _, exit := runCLI(t, "kms", "verify",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view kmsVerifyView
	bodyOf(t, stdout, &view)
	if view.Considered != 2 || view.OK != 2 {
		t.Errorf("counts = %+v", view)
	}
	if view.UnwrapFailed != 0 || view.KEKUnknown != 0 {
		t.Errorf("unexpected failures: %+v", view)
	}
}

// TestKmsVerify_UnwrapFailed: a backup wrapped under a DIFFERENT KEK
// than the one in the local keyring → unwrap_failed and exit 9
// (ExitVerifyFailed). The error code is verify.envelope_break.
func TestKmsVerify_UnwrapFailed(t *testing.T) {
	w := newReadWorld(t)
	var realKEK, wrongKEK [encryption.KeyLen]byte
	if _, err := rand.Read(realKEK[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(wrongKEK[:]); err != nil {
		t.Fatal(err)
	}
	// The keyring holds wrongKEK; the backup is wrapped under realKEK.
	installLocalKEK(t, wrongKEK)
	id := commitEncryptedBackup(t, w, "db1", "bad", 1, realKEK, keystore.KEKRefLocal, []byte("oops"))

	stdout, errb, exit := runCLI(t, "kms", "verify",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("exit = %d, want ExitVerifyFailed(%d)\nstderr: %s",
			exit, output.ExitVerifyFailed, errb)
	}
	if !strings.Contains(errb, "verify.envelope_break") {
		t.Errorf("stderr missing verify.envelope_break:\n%s", errb)
	}
	if !strings.Contains(errb, "unwrap_failed") {
		t.Errorf("stderr missing unwrap_failed summary:\n%s", errb)
	}
	// The success body is rendered to stdout even though the run
	// failed — operators get full per-manifest detail without
	// having to re-run.
	var view kmsVerifyView
	if err := unmarshalResultBody(stdout, &view); err != nil {
		t.Fatalf("decode body: %v\nstdout: %s", err, stdout)
	}
	if view.UnwrapFailed != 1 {
		t.Errorf("UnwrapFailed = %d, want 1", view.UnwrapFailed)
	}
	if len(view.Failures) != 1 || view.Failures[0].BackupID != id {
		t.Errorf("Failures = %+v", view.Failures)
	}
	if view.Failures[0].Status != "unwrap_failed" {
		t.Errorf("Status = %q", view.Failures[0].Status)
	}
}

// TestKmsVerify_KEKUnknown: a backup whose KEKRef the resolver can't
// match (anything other than "local:default") → kek_unknown.
func TestKmsVerify_KEKUnknown(t *testing.T) {
	w := newReadWorld(t)
	var kek [encryption.KeyLen]byte
	if _, err := rand.Read(kek[:]); err != nil {
		t.Fatal(err)
	}
	installLocalKEK(t, kek)
	// Wrap under a ref the local resolver doesn't know.
	commitEncryptedBackup(t, w, "db1", "tenant-x", 1, kek, "tenant-x:v3", []byte("data"))

	stdout, errb, exit := runCLI(t, "kms", "verify",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("exit = %d, want ExitVerifyFailed", exit)
	}
	if !strings.Contains(errb, "kek_unknown") {
		t.Errorf("stderr missing kek_unknown summary:\n%s", errb)
	}
	var view kmsVerifyView
	if err := unmarshalResultBody(stdout, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.KEKUnknown != 1 {
		t.Errorf("KEKUnknown = %d, want 1", view.KEKUnknown)
	}
}

// TestKmsVerify_DeploymentFilter: only the named deployment is
// considered.
func TestKmsVerify_DeploymentFilter(t *testing.T) {
	w := newReadWorld(t)
	var kek [encryption.KeyLen]byte
	if _, err := rand.Read(kek[:]); err != nil {
		t.Fatal(err)
	}
	installLocalKEK(t, kek)
	commitEncryptedBackup(t, w, "db1", "a", 1, kek, keystore.KEKRefLocal, []byte("a"))
	commitEncryptedBackup(t, w, "db2", "b", 2, kek, keystore.KEKRefLocal, []byte("b"))

	stdout, _, exit := runCLI(t, "kms", "verify",
		"--repo", w.repoURL, "--deployment", "db1", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view kmsVerifyView
	bodyOf(t, stdout, &view)
	if view.Considered != 1 || view.OK != 1 {
		t.Errorf("filter counts = %+v", view)
	}
	if view.DeploymentFilter != "db1" {
		t.Errorf("DeploymentFilter = %q", view.DeploymentFilter)
	}
}

// TestKmsVerify_Unencrypted: a plain manifest counts as 'unencrypted'
// — this is policy, NOT a break. Exit is 0.
func TestKmsVerify_Unencrypted(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("plain-payload"))

	stdout, _, exit := runCLI(t, "kms", "verify",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d, want 0 (unencrypted is not a break)", exit)
	}
	var view kmsVerifyView
	bodyOf(t, stdout, &view)
	if view.Unencrypted != 1 {
		t.Errorf("Unencrypted = %d, want 1", view.Unencrypted)
	}
	if view.OK != 0 {
		t.Errorf("OK = %d (unencrypted shouldn't count as OK)", view.OK)
	}
}

// TestKmsVerify_KekFile_PerRef: --kek-file + --kek-ref points the
// resolver at an explicit ref (multi-tenant scenario). Manifests with
// other refs land in kek_unknown.
func TestKmsVerify_KekFile_PerRef(t *testing.T) {
	w := newReadWorld(t)
	var kek [encryption.KeyLen]byte
	if _, err := rand.Read(kek[:]); err != nil {
		t.Fatal(err)
	}
	commitEncryptedBackup(t, w, "db1", "tenant-a", 1, kek, "tenant-a:v1", []byte("a"))

	tmp := filepath.Join(t.TempDir(), "tenant-a.kek")
	if err := os.WriteFile(tmp, kek[:], 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, exit := runCLI(t, "kms", "verify",
		"--repo", w.repoURL,
		"--kek-ref", "tenant-a:v1",
		"--kek-file", tmp,
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view kmsVerifyView
	bodyOf(t, stdout, &view)
	if view.OK != 1 {
		t.Errorf("OK = %d, want 1\nbody: %+v", view.OK, view)
	}
	if view.KEKRefFilter != "tenant-a:v1" {
		t.Errorf("KEKRefFilter = %q", view.KEKRefFilter)
	}
}

// TestKmsVerify_HelpDiscoverable: the parent kms --help lists `verify`,
// and verify --help advertises --deployment / --kek-ref / --kek-file.
func TestKmsVerify_HelpDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "kms", "--help")
	if !strings.Contains(stdout, "verify") {
		t.Errorf("kms --help missing verify subcommand:\n%s", stdout)
	}
	stdout, _, _ = runCLI(t, "kms", "verify", "--help")
	for _, want := range []string{
		"--deployment",
		"--kek-ref",
		"--kek-file",
		"envelope",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("kms verify --help missing %q:\n%s", want, stdout)
		}
	}
}

// TestKmsVerify_TextFormat_HumanReadable: -o text renders the
// summary lines on success.
func TestKmsVerify_TextFormat(t *testing.T) {
	w := newReadWorld(t)
	var kek [encryption.KeyLen]byte
	if _, err := rand.Read(kek[:]); err != nil {
		t.Fatal(err)
	}
	installLocalKEK(t, kek)
	commitEncryptedBackup(t, w, "db1", "ok", 1, kek, keystore.KEKRefLocal, []byte("a"))

	stdout, _, exit := runCLI(t, "kms", "verify",
		"--repo", w.repoURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		"kms verify",
		"Considered:",
		"OK:",
		"envelope health: clean",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output missing %q:\n%s", want, stdout)
		}
	}
}

// unmarshalResultBody parses the success-body half of an output.Result
// envelope. Differs from bodyOf in that it does NOT fail on the
// presence of an Error field — kms verify is the dual-emit case where
// success body and error both carry signal.
func unmarshalResultBody(raw string, into any) error {
	var res struct {
		Result *stdjson.RawMessage `json:"result"`
	}
	if err := stdjson.Unmarshal([]byte(raw), &res); err != nil {
		return err
	}
	if res.Result == nil {
		return nil
	}
	return stdjson.Unmarshal(*res.Result, into)
}
