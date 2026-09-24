package pgbackrest

import (
	"strings"
	"testing"
)

func containsArg(args []string, a string) bool {
	for _, x := range args {
		if x == a {
			return true
		}
	}
	return false
}

// pgBackRest's default restore (no --type, or --type=default) replays
// every archived WAL segment. The native default is recovery_target=
// 'immediate' + promote, which silently discards all WAL archived
// after the backup, so the shim must ask for --to-latest.
func TestRestore_DefaultReplaysAllWAL(t *testing.T) {
	for _, typ := range []string{"", "default"} {
		t.Run("type="+typ, func(t *testing.T) {
			got := captureDispatch(t)
			t.Setenv("PGDATA", "/srv/pg")
			globalArgs = pgbackrestArgs{stanza: "db1", repo1Path: "/r", targetType: typ}
			if err := runRestore(globalArgs); err != nil {
				t.Fatal(err)
			}
			if !containsArg(*got, "--to-latest") {
				t.Fatalf("default restore must pass --to-latest; got %v", *got)
			}
		})
	}
}

// --type=immediate is the one spelling that means "stop at
// consistency" — the native default, so no --to-latest.
func TestRestore_ImmediateStopsAtConsistency(t *testing.T) {
	got := captureDispatch(t)
	t.Setenv("PGDATA", "/srv/pg")
	globalArgs = pgbackrestArgs{stanza: "db1", repo1Path: "/r", targetType: "immediate"}
	if err := runRestore(globalArgs); err != nil {
		t.Fatal(err)
	}
	for _, a := range *got {
		if strings.HasPrefix(a, "--to") {
			t.Fatalf("immediate must not arm a target or --to-latest; got %v", *got)
		}
	}
}

// A targeted restore must not also carry --to-latest (native refuses
// the combination).
func TestRestore_TargetedRestoreHasNoToLatest(t *testing.T) {
	got := captureDispatch(t)
	t.Setenv("PGDATA", "/srv/pg")
	globalArgs = pgbackrestArgs{stanza: "db1", repo1Path: "/r", targetLSN: "0/3000028"}
	if err := runRestore(globalArgs); err != nil {
		t.Fatal(err)
	}
	if containsArg(*got, "--to-latest") {
		t.Fatalf("targeted restore carries --to-latest: %v", *got)
	}
}

// --type=standby must build a hot standby (native `standby create`),
// never a restore that promotes into a divergent primary.
func TestRestore_TypeStandbyBuildsStandby(t *testing.T) {
	got := captureDispatch(t)
	t.Setenv("PGDATA", "/srv/pg")
	globalArgs = pgbackrestArgs{stanza: "db1", repo1Path: "/r", targetType: "standby"}
	if err := runRestore(globalArgs); err != nil {
		t.Fatal(err)
	}
	want := []string{"standby", "create", "db1", "--deployment", "db1", "--backup", "latest", "--target", "/srv/pg", "--repo", "file:///r"}
	if strings.Join(*got, " ") != strings.Join(want, " ") {
		t.Fatalf("standby dispatch:\n got %v\nwant %v", *got, want)
	}
}

// Types with no native equivalent refuse loudly instead of silently
// degrading (xid used to be read as a TIME).
func TestRestore_UnsupportedTypesRefused(t *testing.T) {
	for _, typ := range []string{"xid", "preserve", "none", "bogus"} {
		t.Run(typ, func(t *testing.T) {
			got := captureDispatch(t)
			t.Setenv("PGDATA", "/srv/pg")
			globalArgs = pgbackrestArgs{stanza: "db1", repo1Path: "/r", targetType: typ, target: "12345"}
			err := runRestore(globalArgs)
			if err == nil {
				t.Fatalf("--type=%s accepted; dispatched %v", typ, *got)
			}
			if len(*got) != 0 {
				t.Fatalf("--type=%s dispatched %v despite refusal", typ, *got)
			}
			if typ == "xid" && !strings.Contains(err.Error(), "transaction-ID") {
				t.Errorf("--type=xid refusal should explain the missing XID target; got %v", err)
			}
			if ExitCode(err) != notImplementedExitCode {
				t.Errorf("--type=%s: exit %d, want %d", typ, ExitCode(err), notImplementedExitCode)
			}
		})
	}
}

// pgBackRest hands --target-time to PostgreSQL, which resolves an
// offset-less literal in the server's TimeZone; native reads it as
// UTC. The shim must refuse rather than overshoot the PITR point.
func TestRestore_OffsetLessTimeRefused(t *testing.T) {
	for _, a := range []pgbackrestArgs{
		{targetTime: "2026-04-27 09:42:00"},
		{target: "2026-04-27 09:42:00", targetType: "time"},
		{target: "2026-04-27 09:42"},
	} {
		got := captureDispatch(t)
		t.Setenv("PGDATA", "/srv/pg")
		a.stanza, a.repo1Path = "db1", "/r"
		globalArgs = a
		err := runRestore(globalArgs)
		if err == nil || !strings.Contains(err.Error(), "UTC offset") {
			t.Fatalf("%+v: expected offset refusal, got %v (dispatched %v)", a, err, *got)
		}
	}
	got := captureDispatch(t)
	t.Setenv("PGDATA", "/srv/pg")
	globalArgs = pgbackrestArgs{stanza: "db1", repo1Path: "/r", targetTime: "2026-04-27 09:42:00+02"}
	if err := runRestore(globalArgs); err != nil {
		t.Fatalf("explicit offset refused: %v", err)
	}
	if !sliceContainsPair(*got, "--to", "2026-04-27 09:42:00+02") {
		t.Fatalf("got %v", *got)
	}
}
