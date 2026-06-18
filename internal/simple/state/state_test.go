package state_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/simple/state"
)

// TestLoad_MissingFileReturnsEmpty: first-run case — the cache file
// doesn't exist yet, so Load should hand back a zero-valued State
// with just the current Schema stamped.  Anything else (an error,
// a parse failure) would break the first time the operator starts
// the binary.
func TestLoad_MissingFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	s, err := state.Load(dir)
	if err != nil {
		t.Fatalf("Load on empty dir: %v", err)
	}
	if s == nil {
		t.Fatal("Load returned nil State")
	}
	if s.Schema != state.Schema {
		t.Errorf("Schema = %q, want %q", s.Schema, state.Schema)
	}
	if s.LastDeployment != "" || s.LastRepoURL != "" {
		t.Errorf("expected zero-valued State; got %+v", s)
	}
}

// TestSaveLoad_RoundTrip: write all four fields, read them back,
// they match.  Covers the happy path the dispatch loop hits between
// every successful flow.
func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := &state.State{
		LastDeployment:   "db1",
		LastRepoURL:      "file:///srv/repo",
		LastPGConnection: "postgres://postgres@127.0.0.1/db1",
		LastTargetDir:    "/var/restore",
	}
	if err := state.Save(dir, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := state.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.LastDeployment != in.LastDeployment ||
		out.LastRepoURL != in.LastRepoURL ||
		out.LastPGConnection != in.LastPGConnection ||
		out.LastTargetDir != in.LastTargetDir {
		t.Errorf("round-trip mismatch:\n  in:  %+v\n  out: %+v", in, out)
	}
	if out.Schema != state.Schema {
		t.Errorf("Schema not auto-stamped: %q", out.Schema)
	}
}

// TestSave_AtomicViaRename: a half-written cache file (binary
// crashed mid-Save) must not leave the operator with an unparseable
// simple.yaml that blows up the next Load.  Save writes to .tmp +
// renames, so an interrupted write leaves only the OLD file (or no
// file at all on first save).  We verify the .tmp leftover is
// removed on success.
func TestSave_AtomicViaRename(t *testing.T) {
	dir := t.TempDir()
	if err := state.Save(dir, &state.State{LastDeployment: "db1"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, state.FileName+".tmp")); !os.IsNotExist(err) {
		t.Errorf("leftover .tmp file after successful Save: err=%v", err)
	}
}

// TestSave_CreatesConfigDirIfMissing: first-run operator hasn't
// created ~/.config/pg_hardstorage/ yet.  Save mkdirs it with 0700
// posture (same as the keyring sibling).
func TestSave_CreatesConfigDirIfMissing(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "config", "pg_hardstorage")
	if err := state.Save(dir, &state.State{LastDeployment: "db1"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat created dir: %v", err)
	}
	if !info.IsDir() {
		t.Error("Save target is not a dir")
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o, want 0700", info.Mode().Perm())
	}
}

// TestLoad_BadYAMLPropagates: a corrupted cache file surfaces as an
// error rather than being silently dropped — the caller (main.go)
// fails the session so the operator notices and can fix or delete
// the file.
func TestLoad_BadYAMLPropagates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, state.FileName), []byte("this isn't yaml: {"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Load(dir); err == nil {
		t.Fatal("expected parse error; got nil")
	}
}
