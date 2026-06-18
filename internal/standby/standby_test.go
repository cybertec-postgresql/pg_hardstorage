package standby_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/standby"
)

// TestManager_NewIsEmpty asserts a fresh state file behaves as zero
// standbys without erroring.
func TestManager_NewIsEmpty(t *testing.T) {
	m := standby.NewManager(filepath.Join(t.TempDir(), "standbys.json"), "/usr/bin/pg_hardstorage")
	out, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("fresh state should be empty; got %+v", out)
	}
}

// TestManager_DestroyMissing returns ErrNotFound.
func TestManager_DestroyMissing(t *testing.T) {
	m := standby.NewManager(filepath.Join(t.TempDir(), "standbys.json"), "/usr/bin/pg_hardstorage")
	err := m.Destroy(context.Background(), "ghost", standby.DestroyOptions{})
	if !errors.Is(err, standby.ErrNotFound) {
		t.Errorf("expected ErrNotFound; got %v", err)
	}
}

// TestStateFile_RoundTrip asserts that a hand-written state file is
// readable and that a save followed by a fresh load returns equivalent
// data.
//
// We don't go through Create here (it requires a real repo + verifier);
// the round-trip exercises just the state-file plumbing.
func TestStateFile_RoundTrip(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "standbys.json")
	m := standby.NewManager(statePath, "/usr/bin/pg_hardstorage")

	// First load (file absent) returns empty without error.
	if _, err := m.List(); err != nil {
		t.Fatalf("first list: %v", err)
	}

	// Hand-write a state file with one entry, re-open via a fresh
	// manager, assert it round-trips.
	body := []byte(`{
  "schema": "pg_hardstorage.standbys.v1",
  "standbys": [
    {
      "name": "primary-readonly",
      "deployment": "db1",
      "repo_url": "file:///srv/repo",
      "backup_id": "db1.full.20260427T0900Z",
      "target_dir": "/var/lib/pg/standby",
      "pg_version": 170000,
      "created_at": "2026-04-27T09:15:00Z"
    }
  ]
}
`)
	if err := writeFile(t, statePath, body); err != nil {
		t.Fatal(err)
	}
	m2 := standby.NewManager(statePath, "/usr/bin/pg_hardstorage")
	out, err := m2.List()
	if err != nil {
		t.Fatalf("second list: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 standby; got %d", len(out))
	}
	if out[0].Name != "primary-readonly" || out[0].Deployment != "db1" {
		t.Errorf("unexpected fields: %+v", out[0])
	}
}

// writeFile is a tiny helper to write a fixture without pulling in
// os.WriteFile chatter at every call site.
func writeFile(t *testing.T, path string, body []byte) error {
	t.Helper()
	return osWriteFile(path, body, 0o600)
}
