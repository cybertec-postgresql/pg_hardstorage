package config_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

// patroniBody returns a complete pg_hardstorage.yaml with the
// supplied patroni block on a single deployment. Used by the
// validation tests below.
func patroniBody(patroni string) string {
	return `schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
    patroni:
` + patroni
}

// loadPatroni helper: write the YAML, call Load, return the
// error (nil on success).
func loadPatroni(t *testing.T, body string) error {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), body)
	_, err := config.Load(pathsForTempDir(t, dir))
	return err
}

// TestPatroni_NoConfig_LoadsClean: deployments without a
// patroni block are unaffected by the validator.
func TestPatroni_NoConfig_LoadsClean(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pg_hardstorage.yaml"), `schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
`)
	if _, err := config.Load(pathsForTempDir(t, dir)); err != nil {
		t.Errorf("deployment without patroni should load cleanly; got %v", err)
	}
}

// TestPatroni_SingleSlot_LoadsClean: Mechanism 2 (single
// patroni.slot) is valid.
func TestPatroni_SingleSlot_LoadsClean(t *testing.T) {
	body := patroniBody(`      url: http://patroni:8008
      slot: pg_hardstorage_db1
`)
	if err := loadPatroni(t, body); err != nil {
		t.Errorf("Mechanism 2 single-slot should load cleanly; got %v", err)
	}
}

// TestPatroni_SlotAndSlots_RefusedAsAmbiguous: setting both
// the single-slot field AND the multi-slot list is mutually
// exclusive.
func TestPatroni_SlotAndSlots_RefusedAsAmbiguous(t *testing.T) {
	body := patroniBody(`      url: http://patroni:8008
      slot: legacy_slot
      slots:
        - name: pg_hardstorage_db1_primary
          role: leader
`)
	err := loadPatroni(t, body)
	if err == nil {
		t.Fatal("setting both slot and slots should refuse")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' message; got %v", err)
	}
}

// TestPatroni_SlotsValidRoles_LoadsClean: leader + replica
// are the only valid Role values; both pass validation.
func TestPatroni_SlotsValidRoles_LoadsClean(t *testing.T) {
	body := patroniBody(`      url: http://patroni:8008
      slots:
        - name: pg_hardstorage_db1_primary
          role: leader
        - name: pg_hardstorage_db1_replica
          role: replica
`)
	if err := loadPatroni(t, body); err != nil {
		t.Errorf("leader+replica role should load; got %v", err)
	}
}

// TestPatroni_SlotsBadRole_RefusedAtLoad: a typo'd role
// surfaces with a clear error mentioning the offending value.
func TestPatroni_SlotsBadRole_RefusedAtLoad(t *testing.T) {
	body := patroniBody(`      url: http://patroni:8008
      slots:
        - name: pg_hardstorage_db1_primary
          role: leadr
`)
	err := loadPatroni(t, body)
	if err == nil {
		t.Fatal("invalid role should refuse")
	}
	for _, want := range []string{`"leadr"`, "leader", "replica"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got %v", want, err)
		}
	}
}

// TestPatroni_SlotsEmptyRole_RefusedAtLoad: missing role is
// distinct from invalid role — the message says "role is
// required".
func TestPatroni_SlotsEmptyRole_RefusedAtLoad(t *testing.T) {
	body := patroniBody(`      url: http://patroni:8008
      slots:
        - name: pg_hardstorage_db1_primary
          role: ""
`)
	err := loadPatroni(t, body)
	if err == nil {
		t.Fatal("empty role should refuse")
	}
	if !strings.Contains(err.Error(), "role is required") {
		t.Errorf("expected 'role is required'; got %v", err)
	}
}

// TestPatroni_SlotsEmptyName_RefusedAtLoad: missing name
// surfaces with a clear "name is required" message.
func TestPatroni_SlotsEmptyName_RefusedAtLoad(t *testing.T) {
	body := patroniBody(`      url: http://patroni:8008
      slots:
        - name: ""
          role: leader
`)
	err := loadPatroni(t, body)
	if err == nil {
		t.Fatal("empty name should refuse")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Errorf("expected 'name is required'; got %v", err)
	}
}

// TestPatroni_SlotsDuplicateName_RefusedAtLoad: two slots
// with the same name surface as a structured error mentioning
// the offending name.
func TestPatroni_SlotsDuplicateName_RefusedAtLoad(t *testing.T) {
	body := patroniBody(`      url: http://patroni:8008
      slots:
        - name: pg_hardstorage_db1
          role: leader
        - name: pg_hardstorage_db1
          role: replica
`)
	err := loadPatroni(t, body)
	if err == nil {
		t.Fatal("duplicate name should refuse")
	}
	if !strings.Contains(err.Error(), "duplicate name") {
		t.Errorf("expected 'duplicate name'; got %v", err)
	}
	if !strings.Contains(err.Error(), `"pg_hardstorage_db1"`) {
		t.Errorf("error should name the duplicate; got %v", err)
	}
}
