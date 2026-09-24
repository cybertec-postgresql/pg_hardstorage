//go:build integration

package restore_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/walfetchcmd"
)

// localSource is a throwaway PostgreSQL cluster run from the host's
// binaries (no Docker), reachable only over a private unix socket.
// Used where a test needs things a container makes awkward: a
// tablespace directory the test can inspect, or a server-side
// summarize_wal for incremental backups.
type localSource struct {
	DataDir string
	Sock    string
	DSN     string
	psqlBin string
}

func localBin(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not on PATH (export PATH=/usr/lib/postgresql/<v>/bin:$PATH)", name)
	}
	return p
}

func startLocalSource(t *testing.T, conf ...string) *localSource {
	t.Helper()
	initdb, pgCtl := localBin(t, "initdb"), localBin(t, "pg_ctl")
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
	extra := "listen_addresses = ''\nunix_socket_directories = '" + sock + "'\nport = 5432\n" +
		strings.Join(conf, "\n") + "\n"
	f, err := os.OpenFile(filepath.Join(dir, "postgresql.conf"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(extra)
	_ = f.Close()
	if out, err := exec.Command(pgCtl, "-D", dir, "-l", filepath.Join(tmp, "src.log"), "-w", "start").CombinedOutput(); err != nil {
		log, _ := os.ReadFile(filepath.Join(tmp, "src.log"))
		t.Fatalf("pg_ctl start: %v\n%s\n%s", err, out, log)
	}
	t.Cleanup(func() { _ = exec.Command(pgCtl, "-D", dir, "-m", "immediate", "-w", "stop").Run() })
	return &localSource{
		DataDir: dir,
		Sock:    sock,
		DSN:     fmt.Sprintf("host=%s port=5432 user=postgres dbname=postgres sslmode=disable", sock),
		psqlBin: localBin(t, "psql"),
	}
}

// SQL runs one statement and returns psql's unaligned output.
func (s *localSource) SQL(t *testing.T, sql string) string {
	t.Helper()
	out, err := exec.Command(s.psqlBin, "-X", "-At", "-v", "ON_ERROR_STOP=1",
		"-h", s.Sock, "-p", "5432", "-U", "postgres", "-d", "postgres", "-c", sql).CombinedOutput()
	if err != nil {
		t.Fatalf("psql %q: %v\n%s", sql, err, out)
	}
	return strings.TrimSpace(string(out))
}

// testRepo creates a file:// repository and a signing keypair.
func testRepo(t *testing.T) (string, *backup.Signer, *backup.Verifier) {
	t.Helper()
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	return repoURL, signer, verifier
}

// stubWALFetch points generated restore_commands at a stub that
// always answers "not archived" (wal fetch exit 6). Backups taken with
// IncludeWAL embed what they need to reach consistency, and the test
// binary itself cannot serve `wal fetch`.
func stubWALFetch(t *testing.T) {
	t.Helper()
	stub := filepath.Join(t.TempDir(), "pg_hardstorage")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 6\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(walfetchcmd.RestoreBinEnv, stub)
}
