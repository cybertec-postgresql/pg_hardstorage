// wal_stream_demoted_node_test.go — what happens when the streamer's
// node is demoted and its DSN does not follow the leader.
//
// `wal stream` has no Patroni awareness. Leader-following is delegated
// entirely to libpq: the retry loop's own comment says
// "target_session_attrs=primary routes to the new primary". That works
// only if the operator wrote a multi-host DSN with that parameter. A
// single-host DSN — the shape most people write first, and the shape
// our own archive_command examples use — has nothing to route.
//
// PostgreSQL permits physical replication FROM a standby (cascading
// replication), so a streamer that reconnects to a demoted node is not
// obviously broken. It keeps streaming, keeps committing segments, and
// keeps reporting healthy. What it is actually doing is archiving from
// a replica whose WAL arrives second-hand, while the real primary
// advances and eventually recycles WAL that reached the archive late or
// not at all.
//
// Nothing in the product checks recovery state: pg_is_in_recovery
// appears nowhere in the production tree, and the stream preflight's
// seven checks (wal_level, slots, senders, REPLICATION attribute,
// max_slot_wal_keep_size, idle_replication_slot_timeout) do not include
// "am I talking to a primary".
//
// This test does not assume the outcome. It pins a streamer to one
// node, demotes that node, and records what the product actually does,
// so the assertion below is written against measured behaviour rather
// than against my reading of the code.

//go:build integration && patroni

package topology_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/topology"
)

// TestWalStream_PinnedToADemotedNode is the experiment.
func TestWalStream_PinnedToADemotedNode(t *testing.T) {
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

	cluster, ok := topo.(topology.PatroniCluster)
	if !ok {
		t.Fatal("topology does not expose per-node DSNs")
	}

	// Find the DSN pinned to whichever node currently leads. That is
	// the node we will demote out from under the streamer.
	leaderDSN := topo.ConnString()
	leaderPort := portOf(leaderDSN)
	var pinned string
	for _, d := range cluster.NodeDSNs() {
		if portOf(d) == leaderPort {
			pinned = d
			break
		}
	}
	if pinned == "" {
		t.Fatalf("could not match the leader %s against the per-node DSNs %v",
			redactDSN(leaderDSN), cluster.NodeDSNs())
	}
	t.Logf("streamer pinned to the CURRENT LEADER at port %s (single-host DSN, no target_session_attrs)", leaderPort)

	bin := buildProductBinary(t)
	repoDir := t.TempDir()
	repoURL := "file://" + repoDir
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home)

	initCtx, initCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer initCancel()
	initCmd := exec.CommandContext(initCtx, bin, "init", "--quick",
		"--pg-connection", pinned, "--repo", repoURL)
	initCmd.Env = env
	if out, ierr := initCmd.CombinedOutput(); ierr != nil {
		t.Fatalf("init --quick failed: %v\n%s", ierr, lastLines(string(out), 1500))
	}

	streamCtx, stopStream := context.WithTimeout(ctx, 6*time.Minute)
	defer stopStream()
	logPath := filepath.Join(t.TempDir(), "stream.log")
	streamLog, _ := os.Create(logPath)
	defer streamLog.Close()

	stream := exec.CommandContext(streamCtx, bin, "wal", "stream", "db1",
		"--pg-connection", pinned, "--repo", repoURL, "-o", "text")
	stream.Env = env
	stream.Stdout, stream.Stderr = streamLog, streamLog
	if serr := stream.Start(); serr != nil {
		t.Fatalf("start streamer: %v", serr)
	}
	streamDone := make(chan error, 1)
	go func() { streamDone <- stream.Wait() }()
	defer func() { stopStream(); <-streamDone }()

	// Let it establish before pulling the node out from under it.
	select {
	case err := <-streamDone:
		body, _ := os.ReadFile(logPath)
		t.Fatalf("the streamer exited before the failover: %v\n%s", err, lastLines(string(body), 2000))
	case <-time.After(20 * time.Second):
	}

	// Demote the node the streamer is pinned to.
	_ = switchover(t, ctx, topo, leaderDSN)
	t.Logf("switchover complete; the pinned node is now a REPLICA")

	// Give the streamer time to notice, reconnect, and settle.
	var exited bool
	select {
	case err := <-streamDone:
		exited = true
		t.Logf("streamer EXITED after the demotion: %v", err)
	case <-time.After(90 * time.Second):
		t.Logf("streamer is STILL RUNNING 90s after its node was demoted")
	}

	body, _ := os.ReadFile(logPath)
	log := string(body)

	// Record what actually happened, whatever it was.
	t.Logf("=== measured behaviour ===")
	t.Logf("streamer exited: %v", exited)
	for _, marker := range []string{
		"in_recovery", "not a primary", "standby", "replica",
		"reconnecting", "system_identifier", "wal.stream.starting",
	} {
		t.Logf("  log mentions %-22q : %v", marker, strings.Contains(log, marker))
	}

	// The assertion.
	//
	// Continuing to stream from a demoted node without saying so is the
	// failure worth catching. The streamer reports healthy while the
	// WAL it archives arrives second-hand from a replica; if that
	// replica falls behind or leaves the cluster, the primary recycles
	// WAL that never reached the archive, and the gap is permanent and
	// unannounced.
	//
	// Either outcome is acceptable as long as it is VISIBLE: exiting so
	// a supervisor restarts against the new leader, or continuing while
	// clearly reporting that the source is in recovery.
	saysStandby := strings.Contains(log, "in_recovery") ||
		strings.Contains(log, "not a primary") ||
		strings.Contains(log, "standby")

	// Print the lines that carry the finding, so a passing run leaves
	// the evidence behind rather than just a tick.
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "source_in_recovery") ||
			strings.Contains(line, "in recovery") ||
			strings.Contains(line, "allow-standby-source") {
			t.Logf("  evidence: %s", strings.TrimSpace(line))
		}
	}

	// Retrying is the correct posture, so the streamer must NOT have
	// treated this as fatal: a leader-aware DSN reaches the new primary
	// within a few attempts, and only the pinned case stays stuck.
	if exited {
		t.Errorf("the streamer EXITED on a demoted source. During a failover every node is " +
			"briefly in recovery, so this has to be retryable — exiting turns a transient " +
			"condition into an outage for operators whose DSN would have found the new " +
			"leader on the next attempt.")
	}

	if !exited && !saysStandby {
		t.Errorf("the streamer kept running against a DEMOTED node and never reported "+
			"that its source is in recovery.\n\n"+
			"PostgreSQL allows physical replication from a standby, so this does not fail "+
			"— it silently archives second-hand WAL from a replica while the real primary "+
			"advances. If that replica falls behind or is reinitialised, the primary "+
			"recycles WAL that never reached the archive and the gap is permanent. "+
			"Nothing in the product checks pg_is_in_recovery, and the stream preflight's "+
			"checks do not include 'am I talking to a primary'.\n\nstreamer log:\n%s",
			lastLines(log, 4000))
	}
}

// portOf extracts host:port's port from a libpq URI.
func portOf(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return ""
	}
	rest := dsn[at+1:]
	slash := strings.Index(rest, "/")
	if slash >= 0 {
		rest = rest[:slash]
	}
	colon := strings.LastIndex(rest, ":")
	if colon < 0 {
		return ""
	}
	return rest[colon+1:]
}
