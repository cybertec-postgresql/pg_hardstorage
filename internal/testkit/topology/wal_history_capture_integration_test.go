// wal_history_capture_integration_test.go — a `wal stream`-only
// deployment must produce the timeline-history file a PITR needs to
// cross a failover.
//
// The unit tests for captureStreamTimelineHistory cover its edge cases
// against fakes. They cannot prove the thing that actually matters:
// that a real `wal stream`, pointed at a cluster that has been
// promoted, asks PG for the history file and lands it in the repository
// where `wal fetch` will look for it.
//
// The failure this guards is silent by construction. Our default
// recovery_target_timeline is 'latest', which makes PG follow the
// highest timeline it can resolve a history file FOR. A missing file
// does not fail the restore — PG recovers along the pre-failover
// timeline and promotes a database missing everything written after the
// promotion, reporting success the whole way.
//
// Before this capture existed, the only producer was
// internal/wal/follower.Coordinator, which runs solely under `agent`
// with a Patroni URL configured. A streaming-only HA deployment runs
// neither that nor an archive_command, so nothing wrote the file at
// all.

//go:build integration && patroni

package topology_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/topology"
)

// TestWalStream_CapturesTimelineHistoryAcrossAPromotion is the
// end-to-end proof.
//
// It promotes FIRST and streams SECOND, which is deliberately the
// harder and more common case: an agent that was not running when the
// failover happened. A capture that only worked for a streamer which
// witnessed the promotion live would miss every restart, every deploy,
// and every cold start after an incident.
func TestWalStream_CapturesTimelineHistoryAcrossAPromotion(t *testing.T) {
	topo, err := topology.Build("patroni-local-docker")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := topo.Up(ctx, topology.UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		dctx, c := context.WithTimeout(context.Background(), 2*time.Minute)
		defer c()
		_ = topo.Down(dctx)
	}()

	bin := buildProductBinary(t)
	repoDir := t.TempDir()
	repoURL := "file://" + repoDir
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home)

	runBin := func(timeout time.Duration, args ...string) (string, int) {
		cctx, c := context.WithTimeout(ctx, timeout)
		defer c()
		cmd := exec.CommandContext(cctx, bin, args...)
		cmd.Env = env
		out, rerr := cmd.CombinedOutput()
		code := 0
		if ee, ok := rerr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if rerr != nil {
			code = -1
		}
		return string(out), code
	}

	dsn := topo.ConnString()
	if out, code := runBin(3*time.Minute, "init", "--quick",
		"--pg-connection", dsn, "--repo", repoURL); code != 0 {
		t.Fatalf("init --quick failed (%d):\n%s", code, lastLines(out, 1500))
	}

	// Promote. The cluster is on timeline 2 afterwards, and PG now has
	// a 00000002.history describing where it branched.
	newDSN := switchover(t, ctx, topo, dsn)
	t.Logf("cluster promoted; streaming against %s", redactDSN(newDSN))

	// Stream against the promoted leader. The stream runs until
	// cancelled, so start it and poll for the artefact.
	streamCtx, stopStream := context.WithTimeout(ctx, 3*time.Minute)
	defer stopStream()
	logPath := filepath.Join(t.TempDir(), "stream.log")
	streamLog, _ := os.Create(logPath)
	defer streamLog.Close()

	stream := exec.CommandContext(streamCtx, bin, "wal", "stream", "db1",
		"--pg-connection", newDSN, "--repo", repoURL, "-o", "text")
	stream.Env = env
	stream.Stdout, stream.Stderr = streamLog, streamLog
	if err := stream.Start(); err != nil {
		t.Fatalf("start streamer: %v", err)
	}
	defer func() {
		stopStream()
		_ = stream.Wait()
	}()

	// wal/<dep>/timelines/<tli>.history, keyed by DECIMAL timeline —
	// the hex archive name PG asks for is translated on the fetch side.
	want := filepath.Join(repoDir, "wal", "db1", "timelines", "2.history")

	deadline := time.Now().Add(2 * time.Minute)
	var found bool
	for time.Now().Before(deadline) {
		if _, serr := os.Stat(want); serr == nil {
			found = true
			break
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			t.Fatal("context cancelled while waiting for the history file")
		}
	}

	if !found {
		body, _ := os.ReadFile(logPath)
		t.Fatalf("the streamer never wrote %s.\n\n"+
			"A `wal stream`-only deployment has no other producer of this file: the "+
			"follower Coordinator runs solely under `agent` with Patroni configured, and "+
			"a streaming-only HA setup has no archive_command either. Without it, a PITR "+
			"that needs to cross this promotion cannot resolve the branch point — and "+
			"because our default recovery_target_timeline is 'latest', PG does not fail. "+
			"It recovers along timeline 1 and promotes a database missing everything "+
			"written after the switchover, reporting success.\n\nstreamer log:\n%s",
			want, lastLines(string(body), 3000))
	}

	body, rerr := os.ReadFile(want)
	if rerr != nil {
		t.Fatalf("read %s: %v", want, rerr)
	}
	if len(body) == 0 {
		t.Fatal("the history file is EMPTY. An empty file is worse than a missing one: " +
			"the fetch path finds it, PG parses no branch point from it, and the failure " +
			"moves from 'file not found' to 'recovered along the wrong timeline'.")
	}
	// A real PG history line is `<parentTLI>\t<switchpoint LSN>\t<reason>`.
	if !strings.Contains(string(body), "\t") {
		t.Errorf("history file does not look like PG's format (no tab):\n%q", body)
	}
	t.Logf("captured %s (%d bytes): %q", filepath.Base(want), len(body), strings.TrimSpace(string(body)))
}

// redactDSN strips the password so a DSN can go in the test log.
func redactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}
	scheme := strings.Index(dsn, "://")
	if scheme < 0 {
		return dsn
	}
	return dsn[:scheme+3] + "***" + dsn[at:]
}

// buildProductBinary returns a pg_hardstorage binary to drive.
//
// Deliberately separate from the chaos soak's own builder rather than
// shared with it: that one lives behind the `chaos` tag and carries its
// own PGHS_CHAOS_BIN override, and moving it would couple two lanes
// that currently fail independently.
func buildProductBinary(t *testing.T) string {
	t.Helper()
	root := repoRootForTopologyTests(t)
	// ALWAYS build from the working tree. Preferring a prebuilt
	// bin/pg_hardstorage is how this test first "ran": it picked up a
	// binary from the previous day, exercised code that predated the
	// fix under test, and reported a failure that had nothing to do
	// with the change. A stale artefact makes an integration test
	// answer a question nobody asked, in either direction — it can just
	// as easily go green on code that is no longer there.
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("no pg_hardstorage binary and no Go toolchain to build one: %v\n\n"+
			"This test drives the shipped CLI on purpose — the capture it verifies lives "+
			"in the stream's setup path, and calling into the package would prove the "+
			"helper works rather than that the command runs it.", err)
	}
	out := filepath.Join(t.TempDir(), "pg_hardstorage")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/pg_hardstorage")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the binary under test failed: %v\n%s", err, b)
	}
	return out
}

// repoRootForTopologyTests walks up from this file to the module root.
func repoRootForTopologyTests(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", ".."))
}

// lastLines returns the final n bytes of s, for log excerpts. The
// internal package has an equivalent; this file is in topology_test and
// cannot see it.
func lastLines(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
