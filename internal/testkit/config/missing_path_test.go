package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
)

// `validate --faults typo.yaml` loaded an empty catalogue when the file
// did not exist, and the soak then ran with no faults at all and passed.
// A path the operator named must exist.
func TestLoadFaults_MissingPathIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "faults-typo.yaml")
	f, err := config.LoadFaults(path)
	if err == nil {
		t.Fatalf("a missing faults file loaded as %d faults; want an error", len(f.Faults))
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error should wrap os.ErrNotExist: %v", err)
	}
}

func TestLoadProfiles_MissingPathIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles-typo.yaml")
	p, err := config.LoadProfiles(path)
	if err == nil {
		t.Fatalf("a missing profiles file loaded as %d profiles; want an error", len(p.Profiles))
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error should wrap os.ErrNotExist: %v", err)
	}
}

// The editor's `add` creates the file, so it alone starts from empty.
func TestLoadOrEmpty_MissingPathIsEmpty(t *testing.T) {
	dir := t.TempDir()
	f, err := config.LoadFaultsOrEmpty(filepath.Join(dir, "faults.yaml"))
	if err != nil || len(f.Faults) != 0 {
		t.Fatalf("LoadFaultsOrEmpty on a new file: %v, %+v", err, f)
	}
	p, err := config.LoadProfilesOrEmpty(filepath.Join(dir, "profiles.yaml"))
	if err != nil || len(p.Profiles) != 0 {
		t.Fatalf("LoadProfilesOrEmpty on a new file: %v, %+v", err, p)
	}
}
