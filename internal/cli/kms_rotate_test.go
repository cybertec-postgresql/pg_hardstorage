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
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// commitEncryptedAtCLI plants an encrypted backup manifest in the
// readWorld's repo, wrapped under kek with kekRef. We reuse the
// existing readWorld signer.
func (w *readWorld) commitEncryptedAtCLI(t *testing.T, deployment, backupID string, kek [encryption.KeyLen]byte, kekRef string, idx int) {
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
	chunkBody := []byte("encrypted-payload-" + backupID)
	info, err := cas.PutChunk(context.Background(), chunkBody)
	if err != nil {
		t.Fatal(err)
	}
	stoppedAt := time.Date(2026, 4, 30, 12, idx, 0, 0, time.UTC)
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         backupID,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        stoppedAt.Add(-30 * time.Second),
		StoppedAt:        stoppedAt,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Encryption: &backup.EncryptionInfo{
			Scheme:          "aes-256-gcm",
			KEKRef:          kekRef,
			WrappedDEK:      base64.StdEncoding.EncodeToString(wrapped),
			EnvelopeVersion: 1,
		},
		Files: []backup.FileEntry{{
			Path: "data/" + backupID,
			Size: int64(len(chunkBody)),
			Mode: 0o600,
			Chunks: []backup.ChunkRef{{
				Hash:   info.Hash,
				Offset: 0,
				Len:    int64(len(chunkBody)),
			}},
		}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
}

// writeKEKFile dumps 32 random bytes to <path> and returns the
// bytes for the test's later verification.
func writeKEKFile(t *testing.T, path string) [encryption.KeyLen]byte {
	t.Helper()
	var k [encryption.KeyLen]byte
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, k[:], 0o600); err != nil {
		t.Fatal(err)
	}
	return k
}

// kekFiles writes paired old/new KEK files into a tempdir and
// returns their paths + bytes.
type kekFixture struct {
	oldPath string
	newPath string
	oldKEK  [encryption.KeyLen]byte
	newKEK  [encryption.KeyLen]byte
}

func newKekFixture(t *testing.T) *kekFixture {
	t.Helper()
	dir := t.TempDir()
	old := filepath.Join(dir, "old.kek")
	new_ := filepath.Join(dir, "new.kek")
	return &kekFixture{
		oldPath: old,
		newPath: new_,
		oldKEK:  writeKEKFile(t, old),
		newKEK:  writeKEKFile(t, new_),
	}
}

// TestKMSRotate_RequiredFlags: missing flags surface as
// usage.missing_flag.
func TestKMSRotate_RequiredFlags(t *testing.T) {
	w := newReadWorld(t)
	for _, args := range [][]string{
		{"kms", "rotate", "-o", "json"},                      // no --repo
		{"kms", "rotate", "--repo", w.repoURL, "-o", "json"}, // no old/new refs
		{"kms", "rotate", "--repo", w.repoURL, "--old-kek-ref", "old", "-o", "json"},
		{"kms", "rotate", "--repo", w.repoURL, "--old-kek-ref", "old", "--new-kek-ref", "new", "-o", "json"}, // no kek files
	} {
		_, _, exit := runCLI(t, args...)
		if exit != int(output.ExitMisuse) {
			t.Errorf("args=%v should exit Misuse; got %d", args, exit)
		}
	}
}

// TestKMSRotate_RejectsSameOldNewRef: --old-kek-ref ==
// --new-kek-ref is a usage error.
func TestKMSRotate_RejectsSameOldNewRef(t *testing.T) {
	w := newReadWorld(t)
	kek := newKekFixture(t)
	_, stderr, exit := runCLI(t,
		"kms", "rotate",
		"--repo", w.repoURL,
		"--old-kek-ref", "v1",
		"--new-kek-ref", "v1",
		"--old-kek-file", kek.oldPath,
		"--new-kek-file", kek.newPath,
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("same-ref should exit Misuse; got %d\n%s", exit, stderr)
	}
}

// TestKMSRotate_RejectsBadKEKFileSize: a KEK file that's not 32
// bytes is a usage error.
func TestKMSRotate_RejectsBadKEKFileSize(t *testing.T) {
	w := newReadWorld(t)
	bad := filepath.Join(t.TempDir(), "bad.kek")
	if err := os.WriteFile(bad, []byte("too short"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(t.TempDir(), "good.kek")
	writeKEKFile(t, good)

	_, stderr, exit := runCLI(t,
		"kms", "rotate",
		"--repo", w.repoURL,
		"--old-kek-ref", "old",
		"--new-kek-ref", "new",
		"--old-kek-file", bad,
		"--new-kek-file", good,
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("bad KEK size should exit Misuse; got %d\n%s", exit, stderr)
	}
	if !strings.Contains(stderr, "expected 32 bytes") {
		t.Errorf("error should explain size: %s", stderr)
	}
}

// TestKMSRotate_DryRunDefault: without --apply, the run is a
// dry-run.
func TestKMSRotate_DryRunDefault(t *testing.T) {
	w := newReadWorld(t)
	kek := newKekFixture(t)
	w.commitEncryptedAtCLI(t, "db1", "db1.full.dry", kek.oldKEK, "test:old", 1)

	stdout, _, exit := runCLI(t,
		"kms", "rotate",
		"--repo", w.repoURL,
		"--old-kek-ref", "test:old",
		"--new-kek-ref", "test:new",
		"--old-kek-file", kek.oldPath,
		"--new-kek-file", kek.newPath,
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("dry-run: exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"dry_run": true`) {
		t.Errorf("expected dry_run=true:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"rotated": 1`) {
		t.Errorf("expected 1 candidate:\n%s", stdout)
	}

	// Verify the manifest is unchanged on disk.
	m, err := w.store.Read(context.Background(), "db1", "db1.full.dry", w.verifier)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if m.Encryption.KEKRef != "test:old" {
		t.Errorf("dry-run mutated KEKRef: %q", m.Encryption.KEKRef)
	}
}

// TestKMSRotate_ApplyRotates: --apply flag rewrites the manifest
// to use the new KEK. Then a Read against the rotated manifest
// works (signature still valid).
func TestKMSRotate_ApplyRotates(t *testing.T) {
	w := newReadWorld(t)
	kek := newKekFixture(t)
	w.commitEncryptedAtCLI(t, "db1", "db1.full.apply", kek.oldKEK, "test:old", 1)

	stdout, _, exit := runCLI(t,
		"kms", "rotate",
		"--repo", w.repoURL,
		"--old-kek-ref", "test:old",
		"--new-kek-ref", "test:new",
		"--old-kek-file", kek.oldPath,
		"--new-kek-file", kek.newPath,
		"--apply",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("apply: exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"rotated": 1`) {
		t.Errorf("expected rotated=1:\n%s", stdout)
	}

	// Verify the manifest is rotated on disk.
	m, err := w.store.Read(context.Background(), "db1", "db1.full.apply", w.verifier)
	if err != nil {
		t.Fatalf("Read after rotation: %v", err)
	}
	if m.Encryption.KEKRef != "test:new" {
		t.Errorf("KEKRef = %q, want test:new", m.Encryption.KEKRef)
	}
	// And the new wrapped DEK unwraps with the new KEK.
	wrapped, err := base64.StdEncoding.DecodeString(m.Encryption.WrappedDEK)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encryption.Unwrap(kek.newKEK, wrapped); err != nil {
		t.Errorf("rotated manifest's wrapped_dek doesn't unwrap with new KEK: %v", err)
	}
}

// TestKMSRotate_AuditEmissionOnApply: --apply with rotations emits
// a kms.rotate audit event.
func TestKMSRotate_AuditEmissionOnApply(t *testing.T) {
	w := newReadWorld(t)
	kek := newKekFixture(t)
	w.commitEncryptedAtCLI(t, "db1", "db1.full.audit", kek.oldKEK, "test:old", 1)

	if _, _, exit := runCLI(t,
		"kms", "rotate",
		"--repo", w.repoURL,
		"--old-kek-ref", "test:old",
		"--new-kek-ref", "test:new",
		"--old-kek-file", kek.oldPath,
		"--new-kek-file", kek.newPath,
		"--apply",
		"-o", "json",
	); exit != int(output.ExitOK) {
		t.Fatal("apply failed")
	}

	// Walk the audit chain.
	stdout, _, exit := runCLI(t,
		"audit", "search",
		"--repo", w.repoURL,
		"--action", "kms.rotate",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("audit search: exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, "kms.rotate") {
		t.Errorf("expected kms.rotate event in chain:\n%s", stdout)
	}
}

// TestKMSRotate_AlreadyRotatedIsClean: re-running a rotation
// against already-rotated manifests is a clean no-op (idempotent
// resume).
func TestKMSRotate_AlreadyRotatedIsClean(t *testing.T) {
	w := newReadWorld(t)
	kek := newKekFixture(t)
	w.commitEncryptedAtCLI(t, "db1", "db1.full.idem", kek.oldKEK, "test:old", 1)

	// First rotation.
	if _, _, exit := runCLI(t,
		"kms", "rotate",
		"--repo", w.repoURL,
		"--old-kek-ref", "test:old",
		"--new-kek-ref", "test:new",
		"--old-kek-file", kek.oldPath,
		"--new-kek-file", kek.newPath,
		"--apply",
		"-o", "json",
	); exit != int(output.ExitOK) {
		t.Fatal("first rotate failed")
	}

	// Second rotation, same args.
	stdout, _, exit := runCLI(t,
		"kms", "rotate",
		"--repo", w.repoURL,
		"--old-kek-ref", "test:old",
		"--new-kek-ref", "test:new",
		"--old-kek-file", kek.oldPath,
		"--new-kek-file", kek.newPath,
		"--apply",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("second rotate (resume): exit=%d\n%s", exit, stdout)
	}
	// Re-marshal compactly to assert.
	var env output.Result
	stdjson.Unmarshal([]byte(stdout), &env)
	body, _ := stdjson.Marshal(env.Result)
	if !strings.Contains(string(body), `"already_rotated":1`) {
		t.Errorf("expected already_rotated=1 on resume:\n%s", body)
	}
	if !strings.Contains(string(body), `"rotated":0`) {
		t.Errorf("expected rotated=0 on resume:\n%s", body)
	}
}

// TestKMSRotate_TextRender confirms the operator-friendly text
// output has the punch-list summary.
func TestKMSRotate_TextRender(t *testing.T) {
	w := newReadWorld(t)
	kek := newKekFixture(t)
	w.commitEncryptedAtCLI(t, "db1", "db1.full.text", kek.oldKEK, "test:old", 1)

	stdout, _, exit := runCLI(t,
		"kms", "rotate",
		"--repo", w.repoURL,
		"--old-kek-ref", "test:old",
		"--new-kek-ref", "test:new",
		"--old-kek-file", kek.oldPath,
		"--new-kek-file", kek.newPath,
		"--apply",
		"-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("text: exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"kms rotate — test:old → test:new",
		"Considered:",
		"rotated:",
		"rotation clean",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text render missing %q:\n%s", want, stdout)
		}
	}
}

// Sanity import to keep repo + casdefault used in unrelated test
// files compiling cleanly.
var _ = repo.HSREPOFilename
var _ = casdefault.New
