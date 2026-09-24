//go:build integration

package cli_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/walfetchcmd"
)

// localPGSource is a throwaway PostgreSQL cluster run from the host's
// binaries (no Docker), reachable only over a private unix socket.
type localPGSource struct {
	DataDir string
	Sock    string
	DSN     string
	psql    string
}

func needLocalBin(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not on PATH (export PATH=/usr/lib/postgresql/<v>/bin:$PATH)", name)
	}
	return p
}

func startLocalPGSource(t *testing.T) *localPGSource {
	t.Helper()
	initdb, pgCtl := needLocalBin(t, "initdb"), needLocalBin(t, "pg_ctl")
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "src")
	if out, err := exec.Command(initdb, "-D", dir, "-U", "postgres", "-A", "trust", "--no-sync").CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, out)
	}
	sock, err := os.MkdirTemp("/tmp", "pghs-src-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sock) })
	f, err := os.OpenFile(filepath.Join(dir, "postgresql.conf"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(f, "listen_addresses = ''\nunix_socket_directories = '%s'\nport = 5432\n", sock)
	_ = f.Close()
	if out, err := exec.Command(pgCtl, "-D", dir, "-l", filepath.Join(tmp, "src.log"), "-w", "start").CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl start: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(pgCtl, "-D", dir, "-m", "immediate", "-w", "stop").Run() })
	return &localPGSource{
		DataDir: dir, Sock: sock,
		DSN:  fmt.Sprintf("host=%s port=5432 user=postgres dbname=postgres sslmode=disable", sock),
		psql: needLocalBin(t, "psql"),
	}
}

func (s *localPGSource) SQL(t *testing.T, sql string) string {
	t.Helper()
	out, err := exec.Command(s.psql, "-X", "-At", "-v", "ON_ERROR_STOP=1",
		"-h", s.Sock, "-p", "5432", "-U", "postgres", "-d", "postgres", "-c", sql).CombinedOutput()
	if err != nil {
		t.Fatalf("psql %q: %v\n%s", sql, err, out)
	}
	return strings.TrimSpace(string(out))
}

// stubRestoreBin points generated restore_commands at a stub answering
// "not archived": backups taken with IncludeWAL embed what they need.
func stubRestoreBin(t *testing.T) {
	t.Helper()
	stub := filepath.Join(t.TempDir(), "pg_hardstorage")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 6\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(walfetchcmd.RestoreBinEnv, stub)
}
