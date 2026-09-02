package restore

// tablespace_preflight_scope_test.go — the tablespace preflight guarded
// only the directories the operator had named.
//
// preflightTablespaceTargets iterated remap.AppliedPaths(): the New side
// of each --tablespace-mapping. A tablespace the operator did NOT remap
// is restored to the absolute path recorded in the manifest — the SOURCE
// host's path — and that path can exist on this host: a re-restore into
// the same layout, a staging refresh, or two clusters sharing a
// conventional location like /var/lib/postgresql/tablespaces/fast.
//
// Those destinations were completely unchecked:
//
//   - without --force the restore silently wrote over whatever was
//     there, so the SAFE invocation could still clobber tablespace data
//     while PGDATA itself was correctly refused;
//   - with --force the directory was not cleared, so files from the
//     previous occupant survived alongside the restored ones — the
//     mixed, silently-corrupt result that clearing the main target
//     exists to prevent, and which clearDirContents' own comment calls
//     out as "stale files from a previous occupant mixed with the
//     restore: a silently-corrupt datadir";
//   - and nothing noticed a LIVE cluster, because the running-postmaster
//     check reads postmaster.pid from PGDATA and a tablespace directory
//     does not have one.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func manifestWithTablespace(location string) *backup.Manifest {
	return &backup.Manifest{
		Tablespaces: []backup.Tablespace{
			{OID: 1663, Location: "pg_default"}, // the default: not external
			{OID: 16400, Location: location},
		},
	}
}

func TestTablespaceDestinations_IncludesUnremappedTablespaces(t *testing.T) {
	orig := "/mnt/fast/ts16400"
	got := tablespaceDestinations(manifestWithTablespace(orig), nil)

	var found bool
	for _, d := range got {
		if d == orig {
			found = true
		}
	}
	if !found {
		t.Fatalf("destinations = %v, missing the un-remapped tablespace's own path %q.\n\n"+
			"That path is where the restore writes, and guarding only --tablespace-mapping "+
			"targets left it unchecked: no emptiness refusal, no --force requirement, no "+
			"clearing.", got, orig)
	}
	// pg_default is inside PGDATA, not an external directory.
	for _, d := range got {
		if d == "pg_default" {
			t.Errorf("the default tablespace was treated as an external destination")
		}
	}
}

// A remap target the manifest does not use is still a directory the
// operator named as a destination, so it stays guarded.
func TestTablespaceDestinations_KeepsNamedRemapTargets(t *testing.T) {
	got := tablespaceDestinations(manifestWithTablespace("/mnt/a"),
		TablespaceRemap{{Old: "/unused", New: "/mnt/named"}})
	var sawNamed bool
	for _, d := range got {
		if d == "/mnt/named" {
			sawNamed = true
		}
	}
	if !sawNamed {
		t.Errorf("destinations = %v, dropped the operator-named remap target", got)
	}
}

// The end-to-end property: an un-remapped tablespace whose directory
// already holds data is refused without --force, and cleared with it.
func TestPreflightTablespaceTargets_UnremappedPathIsGuarded(t *testing.T) {
	occupied := t.TempDir()
	stale := filepath.Join(occupied, "other_clusters_data")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := manifestWithTablespace(occupied)

	err := preflightTablespaceTargets(tablespaceDestinations(m, nil), false)
	if err == nil {
		t.Fatal("a restore with no --tablespace-mapping silently wrote into an occupied " +
			"tablespace directory.\n\nPGDATA would have been refused; the tablespace was not, " +
			"so the safe invocation clobbers data outside PGDATA.")
	}
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "preflight.tablespace_not_empty" {
		t.Errorf("code = %v, want preflight.tablespace_not_empty", err)
	}

	// With --force the directory is CLEARED, not written over.
	if err := preflightTablespaceTargets(tablespaceDestinations(m, nil), true); err != nil {
		t.Fatalf("--force should clear the tablespace target: %v", err)
	}
	if entries, _ := os.ReadDir(occupied); len(entries) != 0 {
		t.Fatalf("--force left %d stale entry/entries in the tablespace directory; the "+
			"previous occupant's files would be mixed with the restored ones", len(entries))
	}
}

// A relative or empty location is not an external directory and must not
// be dragged into the guard — pg_default lives inside PGDATA.
func TestTablespaceDestinations_IgnoresNonAbsoluteLocations(t *testing.T) {
	m := &backup.Manifest{Tablespaces: []backup.Tablespace{
		{OID: 1663, Location: "pg_default"},
		{OID: 16401, Location: ""},
	}}
	if got := tablespaceDestinations(m, nil); len(got) != 0 {
		t.Errorf("destinations = %v, want none for a manifest with no external tablespace", got)
	}
}
