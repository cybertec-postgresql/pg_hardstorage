// review79_bugs_test.go — regression tests for the review-79 bug sweep:
// tablespace destination resolution (#3), secure chain staging (#44),
// and the checkpoint-identity change that #3 requires.
package restore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// TestTablespaceDestRoots_NonDefaultRoutesToLocation pins the bug-#3
// fix: a non-default tablespace (absolute Location) gets a destination
// entry so its files materialise under that location, while the main
// data directory's implicit tablespaces (OID 0, or a pg_default-style
// non-absolute location) get NONE — their files belong under PGDATA
// root via fileDestRoot's fallback.
func TestTablespaceDestRoots_NonDefaultRoutesToLocation(t *testing.T) {
	m := &backup.Manifest{
		Tablespaces: []backup.Tablespace{
			{OID: 0, Location: ""},                  // main data dir
			{OID: 1663, Location: "pg_default"},     // pseudo, non-absolute
			{OID: 16384, Location: "/srv/ts1"},      // real external tablespace
			{OID: 16385, Location: "/mnt/fast/ts2"}, // real external tablespace
		},
	}
	dests := tablespaceDestRoots(m, nil)
	if _, ok := dests[0]; ok {
		t.Error("OID 0 must not get a destination entry")
	}
	if _, ok := dests[1663]; ok {
		t.Error("non-absolute pg_default location must not get a destination entry")
	}
	if dests[16384] != "/srv/ts1" {
		t.Errorf("dests[16384] = %q, want /srv/ts1", dests[16384])
	}
	if dests[16385] != "/mnt/fast/ts2" {
		t.Errorf("dests[16385] = %q, want /mnt/fast/ts2", dests[16385])
	}
}

// TestTablespaceDestRoots_HonoursRemap pins that an operator's
// --tablespace-mapping redirects the destination the SAME way it
// rewrites tablespace_map, so restored bytes and PG's recovery-time
// pg_tblspc/ symlink agree.
func TestTablespaceDestRoots_HonoursRemap(t *testing.T) {
	m := &backup.Manifest{
		Tablespaces: []backup.Tablespace{{OID: 16384, Location: "/srv/ts1"}},
	}
	remap := TablespaceRemap{{Old: "/srv/ts1", New: "/restore/ts1"}}
	dests := tablespaceDestRoots(m, remap)
	if dests[16384] != "/restore/ts1" {
		t.Errorf("remapped dests[16384] = %q, want /restore/ts1", dests[16384])
	}
}

// TestFileDestRoot_RoutesByOID pins the file-placement decision: OID 0
// → TargetDir; a mapped tablespace OID → its location; an unmapped
// non-zero OID (main-archive-ish) → TargetDir.
func TestFileDestRoot_RoutesByOID(t *testing.T) {
	const target = "/restore/target"
	dests := map[uint32]string{16384: "/srv/ts1"}

	if got, _ := fileDestRoot(target, dests, 0); got != target {
		t.Errorf("OID 0 → %q, want %q", got, target)
	}
	if got, _ := fileDestRoot(target, dests, 16384); got != "/srv/ts1" {
		t.Errorf("OID 16384 → %q, want /srv/ts1", got)
	}
	if got, _ := fileDestRoot(target, dests, 99999); got != target {
		t.Errorf("unmapped OID → %q, want TargetDir fallback %q", got, target)
	}
}

// TestCheckpointKey_DistinctPerTablespace pins that two files sharing a
// relative path but living in different tablespaces get distinct
// checkpoint keys, so a resume never skips a not-yet-written file.
func TestCheckpointKey_DistinctPerTablespace(t *testing.T) {
	a := &backup.FileEntry{Path: "PG_18_x/16384/1259", TablespaceOID: 16384}
	b := &backup.FileEntry{Path: "PG_18_x/16384/1259", TablespaceOID: 16385}
	if checkpointKey(a) == checkpointKey(b) {
		t.Errorf("checkpoint keys collide across tablespaces: %q", checkpointKey(a))
	}
	// OID 0 stays stable/back-compat.
	d := &backup.FileEntry{Path: "PG_VERSION", TablespaceOID: 0}
	if got, want := checkpointKey(d), "0\x00PG_VERSION"; got != want {
		t.Errorf("default checkpoint key = %q, want %q", got, want)
	}
}

// TestSecureStagingDir_CreatesPrivate pins bug-#44: secureStagingDir
// creates a fresh 0700 directory we own.
func TestSecureStagingDir_CreatesPrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "parent", "staging")
	if err := secureStagingDir(root); err != nil {
		t.Fatalf("secureStagingDir: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("staging mode = %#o, want 0700", info.Mode().Perm())
	}
}

// TestSecureStagingDir_ReusesOurOwn0700Dir pins that a pre-existing
// staging dir WE created (owned by us, mode 0700) is accepted — this is
// what makes chain-restore resume work across retries.
func TestSecureStagingDir_ReusesOurOwn0700Dir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "staging")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// Plant a completion marker so we can prove it survives (resume).
	marker := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := secureStagingDir(root); err != nil {
		t.Fatalf("secureStagingDir on our own dir: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("resume marker was wiped: %v", err)
	}
}

// TestSecureStagingDir_RefusesLoosePermsAndSymlink pins bug-#44's core
// guarantee: a pre-existing staging path that is world/group-accessible
// or is a symlink (the shapes a hostile local user would leave) is
// REFUSED rather than adopted.
func TestSecureStagingDir_RefusesLoosePermsAndSymlink(t *testing.T) {
	// World-writable pre-existing dir → refuse.
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(loose, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := secureStagingDir(loose); err == nil {
		t.Error("expected refusal of a 0777 pre-existing staging dir")
	}

	// Symlink where the staging path should be → refuse.
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	if err := secureStagingDir(link); err == nil {
		t.Error("expected refusal of a symlink staging path")
	} else if !strings.Contains(err.Error(), "staging") {
		t.Errorf("unexpected error shape: %v", err)
	}
}
