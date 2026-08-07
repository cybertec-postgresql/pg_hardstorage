// timeline_chain_test.go — the archive must hold the WHOLE timeline
// ancestry, not just the newest link.
//
// captureStreamTimelineHistory stores the history of the timeline the
// stream is currently following. That is enough when a streamer
// witnesses every promotion. It is not enough when one happens while
// the streamer is down — an agent restart, a deploy, a crash — because
// the timeline it never streamed on leaves no history file behind.
//
// Why a hole in the middle of the chain is not merely untidy:
// PostgreSQL discovers the newest timeline by probing restore_command
// for successive history files and stopping at the FIRST miss
// (findNewestTimeLine in xlogrecovery.c). With
// recovery_target_timeline='latest' — our default — a missing
// 00000002.history therefore does not produce an error. PG concludes
// the newest timeline is 1, recovers along it, and promotes a database
// missing everything written on timelines 2 and 3. The operator asked
// for latest and got the oldest, with nothing anywhere saying so.
//
// This test arranges exactly that: two promotions with the streamer
// deliberately absent for the middle one.

//go:build integration && patroni

package topology_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/topology"
)

func TestTimelineChain_SurvivesAPromotionTheStreamerMissed(t *testing.T) {
	topo, err := topology.Build("patroni-local-docker")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	if err := topo.Up(ctx, topology.UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		dctx, c := context.WithTimeout(context.Background(), 2*time.Minute)
		defer c()
		_ = topo.Down(dctx)
	}()

	cluster, ok := topo.(topology.PatroniCluster)
	if !ok {
		t.Fatal("topology does not expose per-node DSNs")
	}
	leaderAware := leaderAwareDSN(cluster.NodeDSNs())

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

	if out, code := runBin(4*time.Minute, "init", "--quick",
		"--pg-connection", leaderAware, "--repo", repoURL); code != 0 {
		t.Fatalf("init --quick failed (%d):\n%s", code, lastLines(out, 1500))
	}

	logDir := t.TempDir()
	runNo := 0
	// startStreamer runs a streamer and returns a stop function, so the
	// test can take it down across a promotion the way a restart would.
	startStreamer := func() func() {
		runNo++
		sctx, stop := context.WithCancel(ctx)
		lp := filepath.Join(logDir, fmt.Sprintf("stream-%d.log", runNo))
		lf, _ := os.Create(lp)
		cmd := exec.CommandContext(sctx, bin, "wal", "stream", "db1",
			"--pg-connection", leaderAware, "--repo", repoURL, "-o", "text")
		cmd.Env = env
		cmd.Stdout, cmd.Stderr = lf, lf
		if serr := cmd.Start(); serr != nil {
			t.Fatalf("start streamer: %v", serr)
		}
		exited := make(chan struct{})
		go func() { _ = cmd.Wait(); close(exited) }()
		var once sync.Once
		return func() {
			once.Do(func() {
				stop()
				<-exited
				_ = lf.Close()
			})
		}
	}

	// --- Timeline 1, streamer present.
	stop1 := startStreamer()
	time.Sleep(10 * time.Second)
	execSQL(t, ctx, topo, `CREATE TABLE chain_proof(id int primary key, note text)`)
	execSQL(t, ctx, topo, `INSERT INTO chain_proof VALUES (1, 'tli-1')`)
	execSQL(t, ctx, topo, `SELECT pg_switch_wal()`)
	tli1 := currentTimeline(t, ctx, topo)
	t.Logf("timeline %d: marker written, streamer running", tli1)

	// --- The streamer goes away FIRST, then the promotion happens.
	//
	// Order matters and an earlier run of this test got it wrong: it
	// stopped the streamer AFTER the switchover returned, which left
	// roughly nine seconds in which the streamer reconnected to the new
	// leader and captured that timeline's history. The test passed
	// while never creating the situation it describes. The middle
	// timeline has to be one no streamer ever connects on.
	stop1()
	t.Logf("STOPPED the streamer before promoting (an agent restart, a deploy, a crash)")

	_ = switchover(t, ctx, topo, topo.ConnString())
	tli2 := currentTimeline(t, ctx, topo)
	t.Logf("promoted to timeline %d with NO streamer running — nothing captures its history", tli2)

	execSQL(t, ctx, topo, `INSERT INTO chain_proof VALUES (2, 'tli-2')`)
	execSQL(t, ctx, topo, `SELECT pg_switch_wal()`)

	// --- Second promotion, still no streamer.
	//
	// Patroni refuses a switchover with no healthy candidate, and right
	// after the first promotion the demoted node is still rewinding and
	// rejoining. Without this wait the second switchover is declined
	// and the timeline never advances — which is what the first run of
	// this test did, reporting "promoted again to timeline 2".
	waitForHealthyCluster(t, ctx, topo, 3, 3*time.Minute)
	_ = switchover(t, ctx, topo, topo.ConnString())
	tli3 := currentTimeline(t, ctx, topo)
	t.Logf("promoted again to timeline %d, still with no streamer running", tli3)

	if tli3 <= tli2 || tli2 <= tli1 {
		t.Fatalf("timelines did not advance twice (%d -> %d -> %d); this test is not "+
			"testing what it claims", tli1, tli2, tli3)
	}

	// --- Streamer returns, on the newest timeline only.
	stop2 := startStreamer()
	defer stop2()
	time.Sleep(15 * time.Second)
	execSQL(t, ctx, topo, `INSERT INTO chain_proof VALUES (3, 'tli-3')`)
	execSQL(t, ctx, topo, `SELECT pg_switch_wal()`)
	execSQL(t, ctx, topo, `SELECT pg_switch_wal()`)
	waitForArchivedTimeline(t, ctx, repoDir, tli3, 3*time.Minute)
	stop2()

	// Report what the returning streamer actually did. Two promotions
	// went by unwatched, so the WAL between the last archived segment
	// and the new leader's position may be gone for good — in which
	// case refusing loudly is the correct outcome and this is where the
	// evidence for that should live, rather than in an assumption.
	if body, rerr := os.ReadFile(filepath.Join(logDir, "stream-2.log")); rerr == nil {
		for _, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, "start_before_slot_restart_lsn") ||
				strings.Contains(line, "resume_strategy") ||
				strings.Contains(line, "history_captured") ||
				strings.Contains(line, "source_in_recovery") {
				t.Logf("  streamer: %s", strings.TrimSpace(line))
			}
		}
	}

	// --- The assertion.
	//
	// Every history file from 2 up to the newest must be fetchable
	// under the eight-hex-digit name PG asks for. PG probes them in
	// ascending order and stops at the first miss, so a hole anywhere
	// truncates the chain at that point — it does not merely lose the
	// one timeline.
	var missing []string
	for tli := tli1 + 1; tli <= tli3; tli++ {
		name := fmt.Sprintf("%08X.history", tli)
		target := filepath.Join(t.TempDir(), name)
		out, code := runBin(time.Minute, "wal", "fetch", "db1", name, target, "--repo", repoURL)
		if code != 0 {
			missing = append(missing, name)
			t.Logf("  %s: NOT fetchable (exit %d) %s", name, code, firstLine(out))
			continue
		}
		body, _ := os.ReadFile(target)
		t.Logf("  %s: fetchable (%d bytes)", name, len(body))
	}

	if len(missing) > 0 {
		t.Errorf("the timeline-history chain has holes: %v are not fetchable, though the "+
			"cluster is on timeline %d.\n\n"+
			"PostgreSQL finds the newest timeline by probing restore_command for successive "+
			"history files and stopping at the FIRST miss. With "+
			"recovery_target_timeline='latest' — our default — this does not fail: PG "+
			"concludes the newest timeline is the one before the hole, recovers along it, "+
			"and reports success having dropped everything written on every timeline after. "+
			"The operator asked for latest and got less.\n\n"+
			"Only the timeline being streamed gets its history captured, so a promotion "+
			"that happens while the streamer is down leaves no file for that timeline — and "+
			"one gap truncates the whole chain behind it.",
			missing, tli3)
	}
}

// firstLine trims output to its first line, for compact logging.
func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
