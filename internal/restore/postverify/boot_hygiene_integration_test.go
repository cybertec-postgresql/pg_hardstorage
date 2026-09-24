//go:build integration

package postverify_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/postverify"
)

// localSCRAMBackup produces what a restore hands postverify: a base
// backup (backup_label + embedded WAL, via pg_basebackup from a
// throwaway local cluster) whose pg_hba.conf demands scram-sha-256
// for every connection, local included — the shape of a hardened
// production source. Skips without local PostgreSQL binaries.
func localSCRAMBackup(t *testing.T) string {
	t.Helper()
	bin := func(name string) string {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s not on PATH (export PATH=/usr/lib/postgresql/<v>/bin:$PATH)", name)
		}
		return p
	}
	initdb, pgCtl, basebackup := bin("initdb"), bin("pg_ctl"), bin("pg_basebackup")
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", filepath.Base(name), args, err, out)
		}
	}
	run(initdb, "-D", src, "-U", "postgres", "-A", "trust", "--no-sync")
	sock, err := os.MkdirTemp("/tmp", "pghs-src-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sock) })
	run(pgCtl, "-D", src, "-l", filepath.Join(tmp, "src.log"), "-w",
		"-o", "-p 5432 -c listen_addresses='' -c unix_socket_directories="+sock, "start")
	t.Cleanup(func() { _ = exec.Command(pgCtl, "-D", src, "-m", "immediate", "stop").Run() })
	dir := filepath.Join(tmp, "restored")
	run(basebackup, "-h", sock, "-p", "5432", "-U", "postgres", "-D", dir, "-X", "stream", "-c", "fast", "--no-sync")
	if err := os.WriteFile(filepath.Join(dir, "pg_hba.conf"),
		[]byte("local all all scram-sha-256\nhost all all 127.0.0.1/32 scram-sha-256\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// Regression (M106 + H47): the boot test must
//   - authenticate without the source cluster's pg_hba.conf (a scram-
//     only cluster failed every correct restore: peer/scram for role
//     "postgres" with no password to give), and
//   - leave nothing of its own in the data dir it hands back: no
//     postverify-postgres.log, no appended recovery settings in
//     postgresql.auto.conf, no signal files or postmaster.opts it
//     created.
func TestIntegration_BootTest_PrivateAuthAndNoPollution(t *testing.T) {
	dir := localSCRAMBackup(t)
	autoConfBefore, err := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	if err != nil {
		t.Fatal(err)
	}
	hbaBefore, _ := os.ReadFile(filepath.Join(dir, "pg_hba.conf"))
	filesBefore := listDir(t, dir)

	// Stub agent: `wal fetch` exit 6 is "not in the archive", which
	// walfetchcmd maps to the not-found exit PostgreSQL expects.
	stub := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 6\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := postverify.Verify(ctx, postverify.Options{
		Mode:         postverify.ModeRequired,
		DataDir:      dir,
		StartTimeout: 60 * time.Second,
		// A restore_command that always reports "not archived": the
		// backup embeds the WAL it needs to reach consistency.
		RepoURL:     "file:///nonexistent",
		Deployment:  "db1",
		AgentBinary: stub,
	})
	if err != nil {
		t.Fatalf("boot test failed on a healthy scram-only cluster: %v", err)
	}
	if res.Skipped {
		t.Fatalf("boot test skipped: %s", res.SkipReason)
	}
	if res.QueriesRan < 2 {
		t.Errorf("QueriesRan = %d, want >= 2 (SELECT 1 + catalog probe)", res.QueriesRan)
	}

	autoConfAfter, err := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(autoConfAfter) != string(autoConfBefore) {
		t.Errorf("postgresql.auto.conf changed by the boot test:\nbefore:\n%s\nafter:\n%s", autoConfBefore, autoConfAfter)
	}
	hbaAfter, _ := os.ReadFile(filepath.Join(dir, "pg_hba.conf"))
	if string(hbaAfter) != string(hbaBefore) {
		t.Errorf("pg_hba.conf changed by the boot test")
	}
	before := map[string]bool{}
	for _, n := range filesBefore {
		before[n] = true
	}
	for _, n := range listDir(t, dir) {
		// PostgreSQL itself renames backup_label when recovery
		// consumes it: an effect of booting, exactly what the
		// operator's own first start would leave.
		if !before[n] && n != "backup_label.old" {
			t.Errorf("boot test left %q in the data dir", n)
		}
	}
}
