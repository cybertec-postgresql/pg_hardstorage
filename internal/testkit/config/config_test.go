package config_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/catalog"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
)

// --- Fleet ------------------------------------------------------------

func TestFleet_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.yaml")

	f, err := config.LoadFleet(path) // file doesn't exist yet
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Entries) != 0 {
		t.Fatalf("expected empty fleet on missing file; got %d entries", len(f.Entries))
	}

	must(t, f.AddEntry(config.FleetEntry{
		Name: "u24-pg17", OS: "ubuntu:24.04", PG: "17",
		Arch: "amd64", Count: 3,
	}))
	if err := config.SaveFleet(path, f); err != nil {
		t.Fatal(err)
	}

	f2, err := config.LoadFleet(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f2.Entries) != 1 || f2.Entries[0].Name != "u24-pg17" {
		t.Errorf("round-trip lost entries: %+v", f2.Entries)
	}
	if f2.Schema != config.FleetSchema {
		t.Errorf("schema lost on save: %q", f2.Schema)
	}
}

func TestFleet_DuplicateName(t *testing.T) {
	f, _ := config.LoadFleet("")
	must(t, f.AddEntry(config.FleetEntry{Name: "x", OS: "ubuntu:24.04", PG: "17", Count: 1}))
	err := f.AddEntry(config.FleetEntry{Name: "x", OS: "debian:12", PG: "16", Count: 1})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected dup error; got %v", err)
	}
}

func TestFleet_RemoveEntry(t *testing.T) {
	f, _ := config.LoadFleet("")
	_ = f.AddEntry(config.FleetEntry{Name: "x", OS: "ubuntu:24.04", PG: "17", Count: 1})
	if err := f.RemoveEntry("x"); err != nil {
		t.Fatal(err)
	}
	if err := f.RemoveEntry("x"); err == nil {
		t.Errorf("expected ErrNotFound; got nil")
	}
}

func TestFleet_ReplaceEntry(t *testing.T) {
	f, _ := config.LoadFleet("")
	_ = f.AddEntry(config.FleetEntry{Name: "x", OS: "ubuntu:24.04", PG: "17", Count: 1})
	err := f.ReplaceEntry(config.FleetEntry{Name: "x", OS: "debian:12", PG: "16", Count: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.FindEntry("x"); got.OS != "debian:12" || got.Count != 5 {
		t.Errorf("replace didn't take: %+v", got)
	}
}

func TestFleet_ValidateAgainstCatalog_Happy(t *testing.T) {
	c, _ := catalog.Default()
	f, _ := config.LoadFleet("")
	_ = f.AddEntry(config.FleetEntry{Name: "u24", OS: "ubuntu:24.04", PG: "17", Count: 3})
	_ = f.AddEntry(config.FleetEntry{Name: "deb12", OS: "debian:12", PG: "16", Count: 2, Filesystem: "xfs"})
	if err := f.Validate(c); err != nil {
		t.Errorf("happy path failed: %v", err)
	}
}

func TestFleet_ValidateAgainstCatalog_BadOS(t *testing.T) {
	c, _ := catalog.Default()
	f, _ := config.LoadFleet("")
	_ = f.AddEntry(config.FleetEntry{Name: "x", OS: "ubunt:24.04", PG: "17", Count: 1})
	err := f.Validate(c)
	if err == nil || !strings.Contains(err.Error(), "unknown OS") {
		t.Errorf("expected unknown-OS error; got %v", err)
	}
}

func TestFleet_ValidateAgainstCatalog_BadPG(t *testing.T) {
	c, _ := catalog.Default()
	f, _ := config.LoadFleet("")
	_ = f.AddEntry(config.FleetEntry{Name: "x", OS: "rockylinux:9", PG: "12", Count: 1})
	err := f.Validate(c)
	if err == nil || !strings.Contains(err.Error(), "PG 12") {
		t.Errorf("expected unsupported-PG error; got %v", err)
	}
}

func TestFleet_ValidateAgainstCatalog_PatroniNeedsNodes(t *testing.T) {
	c, _ := catalog.Default()
	f, _ := config.LoadFleet("")
	_ = f.AddEntry(config.FleetEntry{
		Name: "p", OS: "debian:12", PG: "17", Count: 1,
		Role: "patroni-cluster", // missing nodes
	})
	err := f.Validate(c)
	if err == nil || !strings.Contains(err.Error(), "nodes ≥2") {
		t.Errorf("expected patroni nodes error; got %v", err)
	}
}

func TestFleet_ValidateAgainstCatalog_NodesOnlyOnPatroni(t *testing.T) {
	c, _ := catalog.Default()
	f, _ := config.LoadFleet("")
	_ = f.AddEntry(config.FleetEntry{
		Name: "x", OS: "ubuntu:24.04", PG: "17", Count: 1,
		Role: "standalone", Nodes: 3,
	})
	err := f.Validate(c)
	if err == nil || !strings.Contains(err.Error(), "patroni-cluster") {
		t.Errorf("expected nodes-only-on-patroni error; got %v", err)
	}
}

func TestFleet_ValidateAgainstCatalog_BadFilesystem(t *testing.T) {
	c, _ := catalog.Default()
	f, _ := config.LoadFleet("")
	_ = f.AddEntry(config.FleetEntry{
		Name: "x", OS: "ubuntu:24.04", PG: "17", Count: 1,
		Filesystem: "ntfs",
	})
	err := f.Validate(c)
	if err == nil || !strings.Contains(err.Error(), "unknown filesystem") {
		t.Errorf("expected fs error; got %v", err)
	}
}

func TestFleet_EffectiveCount_Patroni(t *testing.T) {
	e := config.FleetEntry{Count: 2, Role: "patroni-cluster", Nodes: 3}
	if got := e.EffectiveContainerCount(); got != 6 {
		t.Errorf("expected 2*3=6 containers; got %d", got)
	}
}

// --- Profiles ---------------------------------------------------------

func TestProfiles_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.yaml")
	p, _ := config.LoadProfiles(path)
	must(t, p.AddProfile(config.Profile{
		Name: "small_oltp", TargetSizeGB: 10,
		ChurnMBPerMin: 100, BackupEvery: "5m",
	}))
	if err := config.SaveProfiles(path, p); err != nil {
		t.Fatal(err)
	}
	p2, _ := config.LoadProfiles(path)
	if len(p2.Profiles) != 1 || p2.Profiles[0].Name != "small_oltp" {
		t.Errorf("round-trip lost profile: %+v", p2.Profiles)
	}
}

func TestProfiles_Validate_BadDuration(t *testing.T) {
	p, _ := config.LoadProfiles("")
	_ = p.AddProfile(config.Profile{Name: "x", TargetSizeGB: 10, BackupEvery: "5x"})
	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), "backup_every") {
		t.Errorf("expected duration error; got %v", err)
	}
}

func TestProfiles_Validate_TargetSize(t *testing.T) {
	p, _ := config.LoadProfiles("")
	_ = p.AddProfile(config.Profile{Name: "x", TargetSizeGB: 0})
	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), "target_size_gb") {
		t.Errorf("expected size error; got %v", err)
	}
}

// --- Faults -----------------------------------------------------------

func TestFaults_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "faults.yaml")
	f, _ := config.LoadFaults(path)
	must(t, f.AddFault(config.Fault{
		Name: "disk_full_repo", Weight: 5,
		Action: "disk_full(target=repo, fill=98%)",
	}))
	if err := config.SaveFaults(path, f); err != nil {
		t.Fatal(err)
	}
	f2, _ := config.LoadFaults(path)
	if len(f2.Faults) != 1 {
		t.Errorf("round-trip lost fault: %+v", f2.Faults)
	}
}

func TestFaults_Validate_UnknownAction(t *testing.T) {
	f, _ := config.LoadFaults("")
	_ = f.AddFault(config.Fault{Name: "x", Weight: 5, Action: "nuke_universe()"})
	err := f.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown fault prefix") {
		t.Errorf("expected unknown-fault-prefix error; got %v", err)
	}
}

func TestFaults_Validate_BadSyntax(t *testing.T) {
	f, _ := config.LoadFaults("")
	// Missing close paren is now caught by the parser-level
	// validation that the inject wiring brought in.
	_ = f.AddFault(config.Fault{Name: "x", Weight: 1, Action: "signal(target=agent, sig=9"})
	err := f.Validate()
	if err == nil || !strings.Contains(err.Error(), "missing trailing ')'") {
		t.Errorf("expected parser error; got %v", err)
	}
}

func TestFaults_Validate_Happy(t *testing.T) {
	f, _ := config.LoadFaults("")
	for _, fault := range []config.Fault{
		{Name: "a", Weight: 5, Action: "disk_full(target=repo)"},
		{Name: "b", Weight: 10, Action: "signal(sig=9, target=agent)"},
		{Name: "c", Weight: 3, Action: "toxiproxy(rate=80%, dur=5m)"},
	} {
		_ = f.AddFault(fault)
	}
	if err := f.Validate(); err != nil {
		t.Errorf("happy path failed: %v", err)
	}
}

// --- helpers ----------------------------------------------------------

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
