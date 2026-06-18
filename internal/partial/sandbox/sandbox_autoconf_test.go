package sandbox

import (
	"strings"
	"testing"
)

// TestBuildSandboxAutoConf_PreservesRestoreCommand is the regression
// guard for issue #96: the sandbox must not discard the restore_command
// the restore step wrote into postgresql.auto.conf, or PG can never
// fetch WAL and the recovery driving a partial dump never finishes.
func TestBuildSandboxAutoConf_PreservesRestoreCommand(t *testing.T) {
	existing := "# restore-written\n" +
		"restore_command = '''pg_hardstorage'' wal fetch ''db1'' %f %p --repo ''file:///b'''\n" +
		"recovery_target = 'immediate'\n"
	sock := "/tmp/sock-dir"

	got := string(buildSandboxAutoConf([]byte(existing), sock))

	// The recovery settings survive verbatim.
	if !strings.Contains(got, "restore_command = ") {
		t.Errorf("restore_command was dropped:\n%s", got)
	}
	if !strings.Contains(got, "recovery_target = 'immediate'") {
		t.Errorf("recovery_target was dropped:\n%s", got)
	}

	// Our socket overrides are present.
	for _, want := range []string{
		"listen_addresses = ''",
		"unix_socket_directories = '/tmp/sock-dir'",
		"port = 5432",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("override %q missing:\n%s", want, got)
		}
	}

	// Last-wins: overrides must come AFTER the existing content so PG
	// applies them on top of (not before) the restore's settings.
	if idx, oidx := strings.Index(got, "restore_command"), strings.Index(got, "unix_socket_directories"); !(idx >= 0 && oidx > idx) {
		t.Errorf("override must appear after restore_command (restore@%d, override@%d):\n%s", idx, oidx, got)
	}
}

// TestBuildSandboxAutoConf_NoExisting: with no prior auto.conf the
// sandbox writes just its overrides (no leading blank line).
func TestBuildSandboxAutoConf_NoExisting(t *testing.T) {
	got := string(buildSandboxAutoConf(nil, "/s"))
	if strings.HasPrefix(got, "\n") {
		t.Errorf("unexpected leading newline:\n%q", got)
	}
	if !strings.Contains(got, "unix_socket_directories = '/s'") {
		t.Errorf("override missing:\n%s", got)
	}
	if strings.Contains(got, "restore_command") {
		t.Errorf("did not expect restore_command with no existing conf:\n%s", got)
	}
}

// TestBuildSandboxAutoConf_AppendsNewlineWhenMissing: an existing
// auto.conf with no trailing newline must not get its last line glued to
// our override comment.
func TestBuildSandboxAutoConf_AppendsNewlineWhenMissing(t *testing.T) {
	got := string(buildSandboxAutoConf([]byte("restore_command = 'x'"), "/s"))
	if strings.Contains(got, "restore_command = 'x'# pg_hardstorage") {
		t.Errorf("override glued onto last line:\n%s", got)
	}
	if !strings.Contains(got, "restore_command = 'x'\n") {
		t.Errorf("expected a newline after the existing last line:\n%q", got)
	}
}

// TestBuildPGDumpArgs_DatabaseIsLastPositional pins issue #97's wiring:
// the chosen database name must be the final positional pg_dump argument
// (pg_dump resolves --table only within that database).
func TestBuildPGDumpArgs_DatabaseIsLastPositional(t *testing.T) {
	args := buildPGDumpArgs("/sock", "alice", "mydb",
		[]string{"public.users", "public.events"}, true)

	if args[len(args)-1] != "mydb" {
		t.Errorf("database must be the last arg; got %q (full: %v)", args[len(args)-1], args)
	}
	if !contains(args, "--data-only") {
		t.Errorf("--data-only missing: %v", args)
	}
	if !contains(args, "--table=public.users") || !contains(args, "--table=public.events") {
		t.Errorf("table args missing: %v", args)
	}
	if !contains(args, "--host=/sock") {
		t.Errorf("host arg missing: %v", args)
	}

	// A different database name flows through unchanged.
	other := buildPGDumpArgs("/sock", "alice", "postgres", []string{"public.t"}, false)
	if other[len(other)-1] != "postgres" {
		t.Errorf("expected default database arg, got %q", other[len(other)-1])
	}
	if contains(other, "--data-only") {
		t.Errorf("--data-only should be absent: %v", other)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
