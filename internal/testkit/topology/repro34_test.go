package topology

// Reproduction harness for issue #34: a continuously-running `wal stream`
// preventing the old leader from completing its demote during a Patroni
// switchover. Unlike the L4 scenario (which stop+starts the streamer to
// model a systemd restart — the very workaround that MASKS this bug),
// this runs the streamer as one long-lived process across the switchover
// and asserts the demoted node returns to a running replica.
//
// Opt-in (needs Docker + the Spilo image):
//
//	PGHS_REPRO34_BIN=/tmp/pghs-fixed go test -tags repro34 \
//	    -run TestRepro34_ContinuousStreamerSwitchover \
//	    -timeout 20m ./internal/testkit/topology/

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestRepro34_ContinuousStreamerSwitchover(t *testing.T) {
	bin := os.Getenv("PGHS_REPRO34_BIN")
	if bin == "" {
		t.Skip("set PGHS_REPRO34_BIN to the pg_hardstorage binary under test")
	}
	repoDir, err := os.MkdirTemp("", "repro34-repo-")
	if err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir

	p := newPatroniLocalDocker()
	ctx := context.Background()
	t.Logf("bringing up 3-node Patroni cluster…")
	if err := p.Up(ctx, UpOptions{PGVersion: "17"}); err != nil {
		t.Fatalf("cluster up: %v", err)
	}
	defer p.Down(context.Background())

	// Multi-host, primary-routed DSN — exactly the issue's service.
	var hosts []string
	for _, n := range p.nodes {
		hosts = append(hosts, fmt.Sprintf("127.0.0.1:%d", n.pgPort))
	}
	dsn := fmt.Sprintf("postgres://postgres:%s@%s/postgres?target_session_attrs=primary&sslmode=disable",
		patroniSuperPassword, strings.Join(hosts, ","))

	leaderBefore := p.findLeaderName(ctx)
	t.Logf("initial leader: %s", leaderBefore)

	// init repo + drive a little WAL so a slot + segments exist.
	run(t, bin, "repo", "init", repoURL)
	psql(t, p, leaderBefore, "create table t(i int); insert into t select generate_series(1,2000);")

	// Continuous streamer — NEVER restarted (the whole point).
	streamCtx, stopStream := context.WithCancel(context.Background())
	defer stopStream()
	stream := exec.CommandContext(streamCtx, bin, "wal", "stream", "test",
		"--pg-connection", dsn, "--repo", repoURL, "-o", "text")
	streamLog, _ := os.Create(repoDir + "/stream.log")
	stream.Stdout, stream.Stderr = streamLog, streamLog
	if err := stream.Start(); err != nil {
		t.Fatalf("start streamer: %v", err)
	}
	t.Logf("streamer pid=%d streaming continuously (no restart)", stream.Process.Pid)
	time.Sleep(8 * time.Second) // let it establish the slot + stream

	// Trigger the switchover via Patroni REST on the current leader.
	t.Logf("triggering switchover away from %s…", leaderBefore)
	if err := p.switchover(ctx, leaderBefore); err != nil {
		t.Fatalf("switchover request: %v", err)
	}

	// THE ASSERTION: the OLD leader must return to a running replica.
	// Bug #34 = it stays stuck in "demote in progress" (not running)
	// because the streamer's reconnect re-arms/holds a walsender that
	// blocks its shutdown, until the streamer is restarted.
	deadline := time.Now().Add(150 * time.Second)
	var lastStates string
	for time.Now().Before(deadline) {
		states, oldRunningReplica := p.demotedNodeRunning(ctx, leaderBefore)
		lastStates = states
		if oldRunningReplica {
			t.Logf("✓ old leader %s rejoined as a running replica: %s", leaderBefore, states)
			stopStream()
			return
		}
		time.Sleep(3 * time.Second)
	}
	// Hung. Dump evidence: streamer log, the stuck node's PG/patroni
	// log, and — via a SIBLING node (the stuck one may not answer) —
	// pg_stat_replication showing the walsender state that blocks it.
	logs, _ := os.ReadFile(repoDir + "/stream.log")
	var stuckContainer string
	for _, n := range p.nodes {
		out, _ := exec.Command("docker", "exec", n.container, "hostname").CombinedOutput()
		if strings.TrimSpace(string(out)) == leaderBefore {
			stuckContainer = n.container
		}
	}
	nodeLog := dockerLogsTail(stuckContainer, 25)
	// PG-process view: is the postmaster in shutdown? is a walsender
	// (from our streamer) still attached, blocking it?
	ps, _ := exec.Command("docker", "exec", stuckContainer, "bash", "-c",
		"ps -eo pid,stat,etime,cmd | grep -iE 'postgres|walsender|checkpoint|startup' | grep -v grep").CombinedOutput()
	// PG server log — where the shutdown-wait reason is printed.
	pglog, _ := exec.Command("docker", "exec", stuckContainer, "bash", "-c",
		"f=$(ls -t /home/postgres/pgdata/pgroot/pg_log/*.csv /home/postgres/pgdata/pgroot/pg_log/*.log /var/log/postgresql/* 2>/dev/null | head -1); echo \"[$f]\"; tail -30 \"$f\" 2>/dev/null").CombinedOutput()
	_ = logs
	t.Fatalf("REPRODUCED #34: old leader %s did NOT return to a running replica within 150s\nfinal cluster states: %s\n\n--- ps (postgres procs on stuck node) ---\n%s\n--- PG server log ---\n%s\n--- patroni log tail ---\n%s",
		leaderBefore, lastStates, string(ps), string(pglog), nodeLog)
}

// --- helpers (test-only, same package) ---

func (p *patroniLocalDocker) findLeaderName(ctx context.Context) string {
	for _, n := range p.nodes {
		body, err := httpGet(fmt.Sprintf("http://127.0.0.1:%d/cluster", n.patroniPort))
		if err != nil {
			continue
		}
		var cl struct {
			Members []struct {
				Name, Role, State string
			} `json:"members"`
		}
		if json.Unmarshal(body, &cl) != nil {
			continue
		}
		for _, m := range cl.Members {
			if m.Role == "leader" && m.State == "running" {
				return m.Name
			}
		}
	}
	return ""
}

func (p *patroniLocalDocker) switchover(ctx context.Context, leader string) error {
	// POST /switchover {leader} to any node's REST API; Patroni picks a
	// candidate. Body-less /failover is rejected, so name the leader.
	for _, n := range p.nodes {
		payload := fmt.Sprintf(`{"leader":%q}`, leader)
		req, _ := http.NewRequest("POST",
			fmt.Sprintf("http://127.0.0.1:%d/switchover", n.patroniPort),
			strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
		return fmt.Errorf("switchover HTTP %d: %s", resp.StatusCode, string(b))
	}
	return fmt.Errorf("no reachable patroni REST endpoint")
}

// demotedNodeRunning reports the whole cluster's member states and
// whether the named old leader is now a running (not-leader) member.
func (p *patroniLocalDocker) demotedNodeRunning(ctx context.Context, oldLeader string) (string, bool) {
	for _, n := range p.nodes {
		body, err := httpGet(fmt.Sprintf("http://127.0.0.1:%d/cluster", n.patroniPort))
		if err != nil {
			continue
		}
		var cl struct {
			Members []struct {
				Name, Role, State string
			} `json:"members"`
		}
		if json.Unmarshal(body, &cl) != nil {
			continue
		}
		var sb strings.Builder
		running := false
		for _, m := range cl.Members {
			fmt.Fprintf(&sb, "%s=%s/%s ", m.Name, m.Role, m.State)
			// A recovered demoted node rejoins as a non-leader in a
			// HEALTHY state — Patroni reports a following replica as
			// "streaming" (or "running"); the #34 hang leaves it stuck
			// at "stopping"/"stopped"/"starting".
			if m.Name == oldLeader && m.Role != "leader" &&
				(m.State == "streaming" || m.State == "running" || m.State == "in archive recovery") {
				running = true
			}
		}
		return strings.TrimSpace(sb.String()), running
	}
	return "", false
}

func httpGet(url string) ([]byte, error) {
	c := http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func run(t *testing.T, bin string, args ...string) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
	}
}

func psql(t *testing.T, p *patroniLocalDocker, leader, sql string) {
	t.Helper()
	out, err := exec.Command("docker", "exec", p.nodes[0].container,
		"psql", "-U", "postgres", "-h", "127.0.0.1", "-c", sql).CombinedOutput()
	if err != nil {
		t.Logf("seed psql (non-fatal): %v\n%s", err, out)
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func dockerLogsTail(container string, n int) string {
	if container == "" {
		return "(container not resolved)"
	}
	out, _ := exec.Command("docker", "logs", "--tail", fmt.Sprintf("%d", n), container).CombinedOutput()
	return string(out)
}
