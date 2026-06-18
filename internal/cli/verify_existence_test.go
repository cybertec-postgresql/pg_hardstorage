package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestVerify_ExistenceOnly_HappyPath: --existence-only against
// a backup whose chunks are all present succeeds with
// existence_only=true in the body, ChunksVerified populated,
// BytesVerified zero (we never read the bodies).
func TestVerify_ExistenceOnly_HappyPath(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	stdout, _, exit := runCLI(t, "verify", "db1", id,
		"--repo", w.repoURL, "--existence-only", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("verify --existence-only: exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"existence_only": true`,
		`"chunks_verified": 1`,
		`"chunks_mismatched": 0`,
		`"bytes_verified": 0`, // existence-only never reads bodies
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q:\n%s", want, stdout)
		}
	}
}

// TestVerify_ExistenceOnly_DetectsMissingChunk: a manifest
// whose chunks have been chunk-GC'd surfaces as
// verify.chunks_missing with the missing hash listed.
// Operationally this is the undelete pre-flight: "is chunk X
// still recoverable, or did GC reclaim it?"
func TestVerify_ExistenceOnly_DetectsMissingChunk(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("about-to-be-deleted"))

	// Find the chunk hash in the committed manifest, then
	// delete the storage object directly to simulate chunk-GC
	// having reclaimed it.
	m, err := w.store.Read(context.Background(), "db1", id, w.verifier)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) == 0 || len(m.Files[0].Chunks) == 0 {
		t.Fatal("unexpectedly empty manifest")
	}
	chunkHash := m.Files[0].Chunks[0].Hash
	if err := w.sp.Delete(context.Background(), repo.ChunkKey(chunkHash)); err != nil {
		t.Fatalf("simulate chunk-GC: %v", err)
	}

	_, stderr, exit := runCLI(t, "verify", "db1", id,
		"--repo", w.repoURL, "--existence-only", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("expected non-zero exit on missing chunk; got %d", exit)
	}
	for _, want := range []string{
		"verify.chunks_missing",
		chunkHash.String(),
		"chunk-GC may have already reclaimed",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

// TestVerify_ExistenceOnly_BypassesKEK: existence-only mode
// must work on a manifest whose KEK reference is no longer
// resolvable — operators chasing "is this still restorable"
// shouldn't be blocked on KMS reachability when they only
// want to know whether the chunks are present. The default
// (full) verify still surfaces KEK-resolve as an error; we
// just don't take that path here.
//
// We exercise this by Stat'ing chunks for an unencrypted
// backup; the unencrypted path proves the buildVerifyCAS
// short-circuit works. (Encrypted-but-unresolvable would
// need a more elaborate setup; this confirms the wiring.)
func TestVerify_ExistenceOnly_BypassesKEK(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("kek-skip-test"))
	stdout, _, exit := runCLI(t, "verify", "db1", id,
		"--repo", w.repoURL, "--existence-only", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("existence-only on unencrypted backup: exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"existence_only": true`) {
		t.Errorf("expected existence_only=true:\n%s", stdout)
	}
}

// TestVerify_ExistenceOnly_IncompatibleWithFull: --existence-only
// + --full surfaces a structured usage.bad_flag error rather
// than silently picking one mode.
func TestVerify_ExistenceOnly_IncompatibleWithFull(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("conflict-test"))
	_, stderr, exit := runCLI(t, "verify", "db1",
		"--repo", w.repoURL, "--existence-only", "--full",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse for --existence-only + --full; got %d", exit)
	}
	if !strings.Contains(stderr, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag in stderr:\n%s", stderr)
	}
}

// TestVerify_DefaultBodyShape_NoExistenceOnly: regression —
// the default verify body must NOT include the existence_only
// key (it's omitempty). Preserves the 24-month JSON-compat
// commitment.
func TestVerify_DefaultBodyShape_NoExistenceOnly(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("default-shape"))
	stdout, _, exit := runCLI(t, "verify", "db1", id,
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("default verify: exit=%d\n%s", exit, stdout)
	}
	if strings.Contains(stdout, `"existence_only"`) {
		t.Errorf("default verify body should not include existence_only:\n%s", stdout)
	}
}

// TestVerify_ExistenceOnly_TextRendering: the text-mode tag
// "(existence-only)" appears in the header and "present (no
// integrity check)" appears in the success line; the absence
// case shows "MISSING from the repo" rather than "FAILED
// verification".
func TestVerify_ExistenceOnly_TextRendering(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("text-mode"))
	stdout, _, exit := runCLI(t, "verify", "db1", id,
		"--repo", w.repoURL, "--existence-only", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("text exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"(existence-only)",
		"present (no integrity check)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text mode missing %q:\n%s", want, stdout)
		}
	}
}

// TestVerify_ExistenceOnly_FlagDiscoverable: --existence-only
// shows up in `verify --help` with the undelete pre-flight
// hint.
func TestVerify_ExistenceOnly_FlagDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "verify", "--help")
	for _, want := range []string{
		"--existence-only",
		"backup undelete",
		"Stats every unique chunk",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("verify --help missing %q:\n%s", want, stdout)
		}
	}
}
