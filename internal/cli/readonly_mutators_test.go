package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func setReadOnly(t *testing.T, repoURL string) {
	t.Helper()
	if _, err := repo.SetMode(context.Background(), repo.SetModeOptions{URL: repoURL, Mode: repo.ModeReadOnly}); err != nil {
		t.Fatal(err)
	}
}

// `rotate --apply` tombstoned backups in a repo flipped to read-only
// (`repo set-mode read-only`) — it never checked the mode. The dry-run
// plan must stay available.
func TestRotateApply_RefusesReadOnlyRepo(t *testing.T) {
	w := newReadWorld(t)
	for i := 0; i < 4; i++ {
		w.commitManifest(t, "db1", i)
	}
	setReadOnly(t, w.repoURL)

	if _, errb, exit := runCLI(t, "rotate", "--repo", w.repoURL, "--policy", "count", "--keep-fulls", "1", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("dry-run on a read-only repo must still work: exit=%d\n%s", exit, errb)
	}
	stdout, errb, exit := runCLI(t, "rotate", "--repo", w.repoURL, "--policy", "count", "--keep-fulls", "1", "--apply", "-o", "json")
	if exit != int(output.ExitConflict) || !strings.Contains(stdout+errb, "conflict.repo_read_only") {
		t.Fatalf("rotate --apply on a read-only repo: exit=%d, want %d conflict.repo_read_only\n%s\n%s",
			exit, output.ExitConflict, stdout, errb)
	}
	n := 0
	for _, err := range w.store.List(context.Background(), "db1", w.verifier) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 4 {
		t.Fatalf("read-only repo lost backups to rotate --apply: %d live, want 4", n)
	}
}

// `kms shred` destroyed the KEK and wrote to the audit chain of a
// read-only repo. It must refuse before any of that.
func TestKmsShred_RefusesReadOnlyRepo(t *testing.T) {
	w := newReadWorld(t)
	keyringDir := resolvedKeyringDir(t)
	if _, _, err := keystore.LoadOrGenerateKEK(keyringDir); err != nil {
		t.Fatal(err)
	}
	setReadOnly(t, w.repoURL)

	stdout, errb, exit := runCLI(t, "kms", "shred",
		"--repo", w.repoURL,
		"--require-approval", "whatever",
		"--confirm-keyring", keyringDir,
		"--yes", "-o", "json")
	if exit != int(output.ExitConflict) || !strings.Contains(stdout+errb, "conflict.repo_read_only") {
		t.Fatalf("kms shred on a read-only repo: exit=%d, want %d conflict.repo_read_only\n%s\n%s",
			exit, output.ExitConflict, stdout, errb)
	}
	if !keystore.KEKExists(keyringDir) {
		t.Fatal("kms shred destroyed the KEK of a read-only repo")
	}
}
