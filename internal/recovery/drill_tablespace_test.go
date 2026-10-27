package recovery_test

// drill_tablespace_test.go — a drill must not write outside the temp
// directory it owns.
//
// The drill reads as non-destructive: it mkdtemps a target, restores
// into it, and removes it afterwards. That holds for the main data
// directory. It does NOT hold for a non-default tablespace, which is
// restored to the absolute path recorded in the manifest — on the host
// the backup came from, the LIVE tablespace. Reported as issue #53:
// "Executing the command on the backup source host will overwrite
// current tablespace files."
//
// `restore` has --tablespace-mapping for exactly this; the drill never
// got it, and DrillOptions had nowhere to put one. So the drill could
// only ever restore to the recorded paths.
//
// Refusing is the right default rather than merely offering the flag:
// `schedule --task drill` runs this unattended, so nobody is watching
// when it happens.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/recovery"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func TestDrill_RefusesNonDefaultTablespaceWithoutMapping(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackupWithTablespaces(t, "db1", time.Now().UTC().Add(-time.Hour), 100,
		"/srv/live/ts_fast")

	restoreCalled := false
	_, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptionsWithStubs(
		recovery.DrillOptions{Verifier: w.verifier, SkipVerifyEntirely: true},
		func(ctx context.Context, opts restore.Options) (*restore.Result, error) {
			restoreCalled = true
			return &restore.Result{}, nil
		},
		nil,
	))
	if err == nil {
		t.Fatal("drill proceeded on a backup with a non-default tablespace and no mapping — " +
			"the restore would write to /srv/live/ts_fast, which on the source host is the " +
			"live tablespace")
	}
	if restoreCalled {
		t.Error("restore was invoked; the refusal must happen BEFORE any bytes are written")
	}
	for _, want := range []string{"/srv/live/ts_fast", "--tablespace-mapping"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q — an operator needs the path at risk and the remedy:\n%v",
				want, err)
		}
	}
}

// With a mapping supplied the drill proceeds, and the remap reaches
// restore — otherwise the flag would be accepted and ignored, which is
// worse than not having it.
func TestDrill_WithMappingProceedsAndPassesRemap(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackupWithTablespaces(t, "db1", time.Now().UTC().Add(-time.Hour), 100,
		"/srv/live/ts_fast")

	remap, err := restore.ParseTablespaceRemap([]string{"/srv/live/ts_fast=/tmp/drill-scratch/ts_fast"})
	if err != nil {
		t.Fatal(err)
	}

	var seen restore.TablespaceRemap
	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptionsWithStubs(
		recovery.DrillOptions{Verifier: w.verifier, SkipVerifyEntirely: true, TablespaceRemap: remap},
		func(ctx context.Context, opts restore.Options) (*restore.Result, error) {
			seen = opts.TablespaceRemap
			return &restore.Result{}, nil
		},
		nil,
	))
	if err != nil {
		t.Fatalf("Drill with a mapping: %v", err)
	}
	if r == nil {
		t.Fatal("no report")
	}
	if seen.Empty() {
		t.Error("the remap did not reach restore.Options — the flag would be accepted and ignored")
	}
}

// A backup with only the default tablespace is unaffected: the guard
// must not turn ordinary drills into failures.
func TestDrill_DefaultTablespaceStillDrills(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackup(t, "db1", time.Now().UTC().Add(-time.Hour), 100)

	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptionsWithStubs(
		recovery.DrillOptions{Verifier: w.verifier, SkipVerifyEntirely: true},
		func(ctx context.Context, opts restore.Options) (*restore.Result, error) {
			return &restore.Result{}, nil
		},
		nil,
	))
	if err != nil {
		t.Fatalf("a default-tablespace backup must still drill: %v", err)
	}
	if r == nil {
		t.Fatal("no report")
	}
}

// TablespaceMapAll covers every tablespace without a per-tablespace entry
// (issue #79): the drill proceeds on a backup with external tablespaces,
// and restore sees one <parent>/<oid> mapping per tablespace — a
// tablespace added after the schedule was written included.
func TestDrill_TablespaceMapAllExpandsPerTablespace(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackupWithTablespaces(t, "db1", time.Now().UTC().Add(-time.Hour), 100,
		"/srv/live/ts_fast", "/srv/live/ts_added_later")

	var seen restore.TablespaceRemap
	_, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptionsWithStubs(
		recovery.DrillOptions{Verifier: w.verifier, SkipVerifyEntirely: true, TablespaceMapAll: t.TempDir() + "/scratch"},
		func(ctx context.Context, opts restore.Options) (*restore.Result, error) {
			seen = opts.TablespaceRemap
			return &restore.Result{}, nil
		},
		nil,
	))
	if err != nil {
		t.Fatalf("Drill with TablespaceMapAll: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("restore saw %d mappings (%+v), want one per tablespace", len(seen), seen)
	}
	for _, e := range seen {
		if !strings.HasPrefix(e.Old, "/srv/live/") || strings.HasPrefix(e.New, "/srv/live/") {
			t.Errorf("mapping %+v does not move the tablespace off its live path", e)
		}
	}
}

func TestDrill_TablespaceMapAllRejectsRelativeParent(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackupWithTablespaces(t, "db1", time.Now().UTC().Add(-time.Hour), 100, "/srv/live/ts_fast")
	_, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptionsWithStubs(
		recovery.DrillOptions{Verifier: w.verifier, SkipVerifyEntirely: true, TablespaceMapAll: "scratch"},
		func(ctx context.Context, opts restore.Options) (*restore.Result, error) {
			t.Error("restore ran with an invalid tablespace parent")
			return &restore.Result{}, nil
		}, nil))
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("err = %v, want an absolute-path refusal", err)
	}
}
