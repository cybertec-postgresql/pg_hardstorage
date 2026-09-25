// Build-tagged integration test: the `wal stream` source guards must run
// on the attempt that FIRST reaches PostgreSQL, not only at startup.
//
// Both scenarios start the streamer while PostgreSQL is unreachable and
// let it through only once the reconnect loop has begun. Before the fix
// the system-identifier guard and the wal_segment_size probe ran once,
// at startup, and a failed connect there disabled them for the life of
// the process: the guard was skipped and the probe assumed 16 MiB.
//
// These use a local PostgreSQL (initdb/pg_ctl on $PATH) rather than
// testkit's container, and a gated TCP proxy in front of it to make
// "unreachable at startup, reachable a moment later" deterministic.
//
//go:build integration

package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// startLocalPG boots a throwaway cluster from the PostgreSQL binaries on
// $PATH, trust-authenticated on 127.0.0.1 with replication enabled.
// Skips when initdb is not installed.
func startLocalPG(t *testing.T, initdbArgs ...string) string {
	t.Helper()
	initdb, err := exec.LookPath("initdb")
	if err != nil {
		t.Skip("initdb not on $PATH; export PATH=/usr/lib/postgresql/<ver>/bin:$PATH")
	}
	pgctl, err := exec.LookPath("pg_ctl")
	if err != nil {
		t.Skip("pg_ctl not on $PATH")
	}
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	args := append([]string{"-D", data, "-U", "hsctl", "--auth=trust", "--no-sync"}, initdbArgs...)
	if out, err := exec.Command(initdb, args...).CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, out)
	}
	port := freeTCPPort(t)
	// Unix socket paths are limited to ~107 bytes; the test TMPDIR
	// (`make test-integration` uses a repo-local one) is too long for it.
	sockDir, err := os.MkdirTemp("/tmp", "phs-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	conf := fmt.Sprintf("\nlisten_addresses = '127.0.0.1'\nport = %d\nunix_socket_directories = '%s'\n"+
		"wal_level = replica\nmax_wal_senders = 10\nmax_replication_slots = 10\nfsync = off\n",
		port, sockDir)
	f, err := os.OpenFile(filepath.Join(data, "postgresql.conf"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(conf); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if out, err := exec.Command(pgctl, "-D", data, "-l", filepath.Join(dir, "log"), "-w", "start").CombinedOutput(); err != nil {
		log, _ := os.ReadFile(filepath.Join(dir, "log"))
		t.Fatalf("pg_ctl start: %v\n%s\n%s", err, out, log)
	}
	t.Cleanup(func() {
		_ = exec.Command(pgctl, "-D", data, "-m", "immediate", "stop").Run()
	})
	return fmt.Sprintf("postgres://hsctl@127.0.0.1:%d/postgres?sslmode=disable", port)
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// gatedProxy forwards TCP to target, but slams the door on the first
// refuseFirst connections — PostgreSQL looks down at startup and comes
// up once the streamer is inside its reconnect loop.
type gatedProxy struct {
	addr     string
	refused  atomic.Int32
	accepted atomic.Int32
}

func startGatedProxy(t *testing.T, target string, refuseFirst int32) *gatedProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &gatedProxy{addr: l.Addr().String()}
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = l.Close(); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			if p.refused.Load() < refuseFirst {
				p.refused.Add(1)
				_ = c.Close()
				continue
			}
			p.accepted.Add(1)
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
			go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
		}
	}()
	return p
}

// proxiedDSN rewrites dsn's host:port to the proxy's.
func proxiedDSN(t *testing.T, dsn string, p *gatedProxy) string {
	t.Helper()
	at := strings.Index(dsn, "@")
	slash := strings.Index(dsn[at:], "/")
	return dsn[:at+1] + p.addr + dsn[at+slash:]
}

// TestIntegration_WalStream_SysIDGuardSurvivesUnreachableStartup: the
// deployment's archive belongs to a DIFFERENT cluster. PostgreSQL is down
// when the streamer starts and up by its first reconnect. The guard must
// still refuse — before the fix the startup IDENTIFY_SYSTEM failed, the
// guard was skipped for good, and the reconnect loop archived the foreign
// cluster's WAL into the old lineage.
func TestIntegration_WalStream_SysIDGuardSurvivesUnreachableStartup(t *testing.T) {
	dsn := startLocalPG(t)
	proxy := startGatedProxy(t, strings.Split(strings.Split(dsn, "@")[1], "/")[0], 2)

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	const oldSys = "1234567890123456789"
	name := walsink.SegmentFileName(1, 7, walsink.SegmentSize)
	m := &walsink.SegmentManifest{
		Schema: walsink.Schema, Deployment: "db1", SystemIdentifier: oldSys,
		Timeline: 1, SegmentNumber: 7, SegmentName: name,
		StartLSN: "0/7000000", EndLSN: "0/8000000", SegmentSize: 16 << 20,
	}
	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Put(context.Background(), walsink.SegmentPath("db1", 1, name),
		bytes.NewReader(raw), storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
		t.Fatal(err)
	}

	walCtx, walCancel := context.WithCancel(context.Background())
	defer walCancel()
	go func() { _ = driveWAL(walCtx, dsn, "wal_test", 2048) }()

	root := cli.NewRoot()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{
		"wal", "stream", "db1",
		"--pg-connection", proxiedDSN(t, dsn, proxy),
		"--repo", repoURL,
		"--once",
		"--output", "json",
	})
	exit := runWalStreamBounded(t, root, walStreamTimeout)
	walCancel()
	if proxy.accepted.Load() == 0 {
		t.Fatalf("the proxy never let a connection through (exit %d) — the scenario did not run\nstderr: %s",
			exit, stderr.String())
	}
	if exit != int(output.ExitPreflight) {
		t.Fatalf("exit = %d, want ExitPreflight(%d): a cluster first reached on a RECONNECT was not "+
			"checked against the deployment's recorded system identifier, and its WAL entered the "+
			"old lineage\nstderr: %s", exit, int(output.ExitPreflight), stderr.String())
	}
	if !strings.Contains(stderr.String(), "preflight.system_identifier_changed") {
		t.Errorf("expected preflight.system_identifier_changed:\n%s", stderr.String())
	}
	// Nothing may have been archived under the foreign identifier.
	for info, lerr := range sp.List(context.Background(), "wal/db1/") {
		if lerr != nil {
			t.Fatal(lerr)
		}
		if strings.HasSuffix(info.Key, ".json") && info.Key != walsink.SegmentPath("db1", 1, name) &&
			!strings.Contains(info.Key, "/timelines/") && !strings.Contains(info.Key, "/gaps/") {
			t.Errorf("foreign cluster's WAL archived at %s", info.Key)
		}
	}
}

// TestIntegration_WalStream_SegSizeProbedOnReconnect: a 64 MiB-segment
// cluster, down at startup. Before the fix the startup probe could not
// connect, silently assumed 16 MiB, and was never repeated — a FRESH
// deployment has nothing for guardSegmentSize to compare against, so the
// whole archive was chopped and named for the wrong size.
func TestIntegration_WalStream_SegSizeProbedOnReconnect(t *testing.T) {
	const segBytes = 64 << 20
	dsn := startLocalPG(t, "--wal-segsize=64")
	proxy := startGatedProxy(t, strings.Split(strings.Split(dsn, "@")[1], "/")[0], 2)

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}

	walCtx, walCancel := context.WithTimeout(context.Background(), walStreamSegBudget+30*time.Second)
	defer walCancel()
	go func() { _ = driveWAL(walCtx, dsn, "wal_test", 8192) }()

	root := cli.NewRoot()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{
		"wal", "stream", "db1",
		"--pg-connection", proxiedDSN(t, dsn, proxy),
		"--repo", repoURL,
		"--once",
		"--output", "json",
	})
	exit := runWalStreamBounded(t, root, walStreamSegBudget)
	walCancel()
	if proxy.refused.Load() < 2 || proxy.accepted.Load() == 0 {
		t.Fatalf("scenario did not run (refused=%d accepted=%d)", proxy.refused.Load(), proxy.accepted.Load())
	}
	if exit != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", exit, stderr.String())
	}

	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	found := false
	for info, lerr := range sp.List(context.Background(), "wal/db1/00000001/") {
		if lerr != nil {
			t.Fatal(lerr)
		}
		if !strings.HasSuffix(info.Key, ".json") || strings.Contains(info.Key, ".json.tmp.") {
			continue
		}
		rc, gerr := sp.Get(context.Background(), info.Key)
		if gerr != nil {
			t.Fatal(gerr)
		}
		raw, _ := io.ReadAll(rc)
		_ = rc.Close()
		sm, perr := walsink.ParseSegmentManifest(raw)
		if perr != nil {
			continue
		}
		found = true
		if sm.SegmentSize != segBytes {
			t.Errorf("segment %s archived with SegmentSize=%d, want %d: the streamer never "+
				"re-probed wal_segment_size after an unreachable startup and chopped a 64 MiB "+
				"cluster's WAL at an assumed 16 MiB", sm.SegmentName, sm.SegmentSize, segBytes)
		}
	}
	if !found {
		t.Fatalf("no committed segment manifest\nstderr: %s", stderr.String())
	}
}
