package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
)

// End-to-end through the real CLI verbs (no PostgreSQL needed): an
// encrypted backup plus a WAL segment archived by `wal push` under the
// local KEK, then a local `kms rotate --apply`, then the operator swaps
// kek.bin for the new key exactly as rotate-kek.md step 3 says. `wal
// fetch` — the restore_command every PITR runs — must still decrypt the
// pre-rotation segment with ONLY the new KEK on disk.
//
// Before the fix, rotation rewrapped the backup manifest and the
// shared-DEK slot but never the segment manifests, whose own envelope
// is what `wal fetch` unwraps: every pre-rotation segment became
// undecryptable and PITR past the base backup was impossible.
func TestKMSRotate_WALSegmentsDecryptWithOnlyTheNewKEK(t *testing.T) {
	w := newReadWorld(t)
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	keyringDir := p.Keyring.Value
	oldKEK, _, err := keystore.LoadOrGenerateKEK(keyringDir)
	if err != nil {
		t.Fatal(err)
	}
	kekPath := filepath.Join(keyringDir, keystore.KEKFileName)

	segmentName := "000000010000000000000005"
	segPath := filepath.Join(t.TempDir(), segmentName)
	body := make([]byte, walsink.SegmentSize)
	for i := range body {
		body[i] = byte((i*13 + 7) % 251)
	}
	if err := os.WriteFile(segPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errb, exit := runCLI(t, "wal", "push", "db1", segPath,
		"--repo", w.repoURL, "--system-identifier", "7000000000000000001", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("wal push exit=%d\n%s", exit, errb)
	}
	w.commitEncryptedAtCLI(t, "db1", "db1.full.pre", oldKEK, keystore.KEKRefLocal, 1)

	newPath := filepath.Join(t.TempDir(), "new.kek")
	newKEK := writeKEKFile(t, newPath)

	stdout, errb, exit := runCLI(t, "kms", "rotate",
		"--repo", w.repoURL,
		"--old-kek-ref", keystore.KEKRefLocal,
		"--new-kek-ref", "local:v2",
		"--old-kek-file", kekPath,
		"--new-kek-file", newPath,
		"--apply", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("kms rotate --apply exit=%d\n%s\n%s", exit, stdout, errb)
	}
	if !strings.Contains(stdout, `"wal_rotated": 1`) {
		t.Errorf("rotation must report the rewrapped WAL segment:\n%s", stdout)
	}

	// Retire the old key: install the new one as kek.bin.
	if err := os.WriteFile(kekPath, newKEK[:], 0o600); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "restored.wal")
	if _, errb, exit := runCLI(t, "wal", "fetch", "db1", segmentName, target,
		"--repo", w.repoURL); exit != int(output.ExitOK) {
		t.Fatalf("wal fetch after rotation exit=%d — pre-rotation WAL is undecryptable with the new KEK (PITR impossible)\n%s", exit, errb)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("fetched segment differs from the archived one")
	}
}

// A dry-run whose plan already contains failures (here: the wrong
// --old-kek-file) used to exit 0 with the failures buried in the body,
// so "preview, then --apply if the preview passed" went ahead with a
// rotation that could never complete.
func TestKMSRotate_DryRunWithFailuresExitsNonZero(t *testing.T) {
	w := newReadWorld(t)
	kek := newKekFixture(t)
	w.commitEncryptedAtCLI(t, "db1", "db1.full.x", kek.oldKEK, "test:old", 1)
	wrongPath := filepath.Join(t.TempDir(), "wrong.kek")
	writeKEKFile(t, wrongPath)

	stdout, stderr, exit := runCLI(t, "kms", "rotate",
		"--repo", w.repoURL,
		"--old-kek-ref", "test:old", "--new-kek-ref", "test:new",
		"--old-kek-file", wrongPath,
		"--new-kek-file", kek.newPath,
		"-o", "json")
	if exit != int(output.ExitError) {
		t.Fatalf("dry-run with a failing plan: exit=%d, want %d:\n%s", exit, output.ExitError, stdout)
	}
	if !strings.Contains(stdout+stderr, "kms.rotate_plan_failed") {
		t.Errorf("want kms.rotate_plan_failed:\n%s\n%s", stdout, stderr)
	}
}
