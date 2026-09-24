package barman

import (
	"bytes"
	"io"
	"testing"
)

func runWALArchive(t *testing.T, argv ...string) (captured []string, err error) {
	t.Helper()
	restore := swapDispatcher(func(_, _ io.Writer, args []string) error {
		captured = append([]string(nil), args...)
		return nil
	})
	defer restore()
	restoreDep := swapDeploymentLookup(func(string) (deploymentSettings, error) {
		return deploymentSettings{Repo: "file:///tmp/test-repo"}, nil
	})
	defer restoreDep()
	var sout, serr bytes.Buffer
	root := NewWALArchiveRoot(&sout, &serr)
	root.SetArgs(argv)
	root.SetOut(&sout)
	root.SetErr(&serr)
	_, err = root.ExecuteC()
	return captured, err
}

// barman-wal-archive's documented options appear in real
// archive_command lines; an unknown-flag failure makes PostgreSQL
// retry forever while WAL piles up.
func TestWALArchiveAcceptsDocumentedFlags(t *testing.T) {
	for _, argv := range [][]string{
		{"-U", "barman", "backup.internal", "db1", "pg_wal/000000010000000000000005"},
		{"--user=barman", "--port", "2222", "backup.internal", "db1", "pg_wal/000000010000000000000005"},
		{"-z", "backup.internal", "db1", "pg_wal/000000010000000000000005"},
		{"--gzip", "--bzip2", "--xz", "--snappy", "--zstd", "--lz4", "db1", "pg_wal/000000010000000000000005"},
		{"-c", "/etc/barman.conf", "backup.internal", "db1", "pg_wal/000000010000000000000005"},
	} {
		got, err := runWALArchive(t, argv...)
		if err != nil {
			t.Fatalf("%v: %v", argv, err)
		}
		want := []string{"wal", "push", "db1", "pg_wal/000000010000000000000005"}
		if s := stripInjectedFlags(got); !equalSlices(s, want) {
			t.Errorf("%v: dispatched %v, want %v", argv, s, want)
		}
	}
}

// --test is barman-wal-archive's connectivity check (run as
// `barman-wal-archive --test HOST SERVER DUMMY`): it must succeed
// against a reachable repository without archiving anything.
func TestWALArchiveTestIsConnectivityCheck(t *testing.T) {
	for _, flag := range []string{"--test", "-t"} {
		got, err := runWALArchive(t, flag, "backup.internal", "db1", "DUMMY")
		if err != nil {
			t.Fatalf("%s: %v", flag, err)
		}
		if len(got) < 2 || got[0] != "list" || got[1] != "db1" || !containsPair(got, "--repo", "file:///tmp/test-repo") {
			t.Fatalf("%s: want a read-only `list db1 --repo ...` probe, got %v", flag, got)
		}
		for _, a := range got {
			if a == "push" || a == "DUMMY" {
				t.Fatalf("%s: connectivity test must not archive: %v", flag, got)
			}
		}
	}
}
