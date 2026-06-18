// essentialfiles_test.go — coverage for CheckEssentialFiles.
//
// Pins the issue #84 contract: when the source PG's data directory
// is missing a critical file at backup time, the resulting manifest
// must be refused at commit time rather than discovered at restore.

package backup

import (
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// manifestWith builds a minimal Manifest whose file list contains
// the named top-level entries.  Each gets a synthetic 1-chunk file
// so Manifest.Validate-style downstream checks see a self-consistent
// shape, but we don't actually run Validate here — only the
// essential-file gate.
func manifestWith(names ...string) *Manifest {
	m := &Manifest{}
	for _, n := range names {
		m.Files = append(m.Files, FileEntry{
			Path: n,
			Size: 1,
			Chunks: []ChunkRef{{
				Hash: repo.Hash{1}, Offset: 0, Len: 1,
			}},
		})
	}
	return m
}

const (
	// All inside the same standard RHEL/Rocky data directory.
	rhelData  = "/var/lib/pgsql/18/data"
	rhelConf  = "/var/lib/pgsql/18/data/postgresql.conf"
	rhelHba   = "/var/lib/pgsql/18/data/pg_hba.conf"
	rhelIdent = "/var/lib/pgsql/18/data/pg_ident.conf"
	// Debian/Ubuntu split layout.
	debData  = "/var/lib/postgresql/18/main"
	debConf  = "/etc/postgresql/18/main/postgresql.conf"
	debHba   = "/etc/postgresql/18/main/pg_hba.conf"
	debIdent = "/etc/postgresql/18/main/pg_ident.conf"
)

// Happy path: every required file is present.  The standard
// RHEL/Rocky layout (issue #84 reporter's case) with all configs
// intact must pass without complaint.
func TestCheckEssentialFiles_AllPresent(t *testing.T) {
	m := manifestWith(
		"PG_VERSION", "postgresql.auto.conf",
		"postgresql.conf", "pg_hba.conf", "pg_ident.conf",
	)
	if err := CheckEssentialFiles(m, rhelData, rhelConf, rhelHba, rhelIdent); err != nil {
		t.Errorf("expected nil; got %v", err)
	}
}

// Issue #84's exact reproducer: PGDATA is missing postgresql.conf
// (deleted by the operator while PG was running).  PG_VERSION etc.
// are still there.  The check MUST flag it.
func TestCheckEssentialFiles_PostgresqlConfMissing_Issue84(t *testing.T) {
	m := manifestWith(
		"PG_VERSION", "postgresql.auto.conf",
		"pg_hba.conf", "pg_ident.conf",
		// Note: no postgresql.conf.
	)
	err := CheckEssentialFiles(m, rhelData, rhelConf, rhelHba, rhelIdent)
	if err == nil {
		t.Fatal("expected an error; got nil (issue #84 regression)")
	}
	var me *MissingEssentialFilesError
	if !errors.As(err, &me) {
		t.Fatalf("error type = %T, want *MissingEssentialFilesError", err)
	}
	if len(me.AlwaysRequired) != 0 {
		t.Errorf("AlwaysRequired = %v, want empty (PG_VERSION + postgresql.auto.conf are present)", me.AlwaysRequired)
	}
	if want := []string{"postgresql.conf"}; len(me.InternalConfigs) != 1 || me.InternalConfigs[0] != "postgresql.conf" {
		t.Errorf("InternalConfigs = %v, want %v", me.InternalConfigs, want)
	}
	if !strings.Contains(err.Error(), "postgresql.conf") {
		t.Errorf("error message should mention postgresql.conf; got %q", err.Error())
	}
}

// PG_VERSION missing: alwaysRequiredFiles set, the most severe
// case (the data dir is fundamentally broken).
func TestCheckEssentialFiles_PGVersionMissing(t *testing.T) {
	m := manifestWith("postgresql.auto.conf", "postgresql.conf", "pg_hba.conf", "pg_ident.conf")
	err := CheckEssentialFiles(m, rhelData, rhelConf, rhelHba, rhelIdent)
	if err == nil {
		t.Fatal("expected an error; got nil")
	}
	var me *MissingEssentialFilesError
	if !errors.As(err, &me) {
		t.Fatalf("error type = %T", err)
	}
	if got := me.AlwaysRequired; len(got) != 1 || got[0] != "PG_VERSION" {
		t.Errorf("AlwaysRequired = %v, want [PG_VERSION]", got)
	}
}

// Debian/Ubuntu split layout: configs live OUTSIDE PGDATA.  A
// missing postgresql.conf from the manifest is then expected (PG
// never streamed it) and must NOT be flagged.
func TestCheckEssentialFiles_DebianSplitLayout_AcceptsMissingConfigs(t *testing.T) {
	// PGDATA carries only the always-required + postgresql.auto.conf.
	m := manifestWith("PG_VERSION", "postgresql.auto.conf")
	if err := CheckEssentialFiles(m, debData, debConf, debHba, debIdent); err != nil {
		t.Errorf("Debian split layout should accept missing-in-manifest configs (they live in /etc); got %v", err)
	}
}

// Mixed: data_directory layout has postgresql.conf inside but
// hba_file points to an external path.  Only the in-PGDATA one is
// required.
func TestCheckEssentialFiles_MixedLayout(t *testing.T) {
	m := manifestWith("PG_VERSION", "postgresql.auto.conf", "postgresql.conf")
	// pg_hba.conf and pg_ident.conf are external.
	err := CheckEssentialFiles(m, rhelData, rhelConf, "/etc/pg/pg_hba.conf", "/etc/pg/pg_ident.conf")
	if err != nil {
		t.Errorf("mixed layout: external hba/ident should not be required; got %v", err)
	}
}

// Multiple missing: the error reports every gap, not just the first.
func TestCheckEssentialFiles_MultipleMissing_ReportsAll(t *testing.T) {
	m := manifestWith("postgresql.auto.conf") // no PG_VERSION, no configs
	err := CheckEssentialFiles(m, rhelData, rhelConf, rhelHba, rhelIdent)
	if err == nil {
		t.Fatal("expected an error")
	}
	var me *MissingEssentialFilesError
	if !errors.As(err, &me) {
		t.Fatalf("error type = %T", err)
	}
	if got, want := me.AlwaysRequired, []string{"PG_VERSION"}; !equalStringSlice(got, want) {
		t.Errorf("AlwaysRequired = %v, want %v", got, want)
	}
	wantConfigs := []string{"pg_hba.conf", "pg_ident.conf", "postgresql.conf"}
	if got := me.InternalConfigs; !equalStringSlice(got, wantConfigs) {
		t.Errorf("InternalConfigs = %v, want %v (alphabetical)", got, wantConfigs)
	}
}

// Defensive cases.
func TestCheckEssentialFiles_NilManifest(t *testing.T) {
	if err := CheckEssentialFiles(nil, rhelData, "", "", ""); err == nil {
		t.Error("expected an error for nil manifest")
	}
}

func TestCheckEssentialFiles_EmptyDataDir(t *testing.T) {
	m := manifestWith("PG_VERSION", "postgresql.auto.conf")
	if err := CheckEssentialFiles(m, "", rhelConf, rhelHba, rhelIdent); err == nil {
		t.Error("expected an error when data_directory is unknown")
	}
}

// insideDataDir corner cases.  filepath.Clean normalises trailing
// slashes and "//" runs.
func TestInsideDataDir(t *testing.T) {
	cases := []struct {
		path string
		dir  string
		want bool
	}{
		{"/var/lib/pgsql/18/data/postgresql.conf", "/var/lib/pgsql/18/data", true},
		{"/var/lib/pgsql/18/data/postgresql.conf", "/var/lib/pgsql/18/data/", true},
		{"/etc/postgresql/18/main/postgresql.conf", "/var/lib/postgresql/18/main", false},
		// Adjacent path with a matching prefix — must NOT match.
		{"/var/lib/pgsql/18/data_archive/postgresql.conf", "/var/lib/pgsql/18/data", false},
		// Exact match (weird input but defined behaviour).
		{"/var/lib/pgsql/18/data", "/var/lib/pgsql/18/data", true},
	}
	for _, c := range cases {
		if got := insideDataDir(c.path, c.dir); got != c.want {
			t.Errorf("insideDataDir(%q, %q) = %v, want %v", c.path, c.dir, got, c.want)
		}
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
