package topology

// Chaos soak with a RESTORE-PROOF gate (integrity program #1).
//
// Runs the production model continuously — an ENCRYPTED repo, a
// continuous `wal stream`, concurrent scheduled backups, constant DB
// churn — over a real 3-node Patroni cluster while injecting a
// randomized fault sequence (switchovers, leader pauses ≈ GC stalls,
// concurrent-backup bursts). The pass criterion is never "exit 0": at
// the end EVERY committed backup must verify (--full) AND restore, the
// WAL lineage must be gap-free, and exactly one shared-DEK object must
// exist. This is the harness shape that would have caught both #31
// (concurrent-writer DEK fork) and #34 (switchover hang) before users
// did.
//
// Rules encoded from the post-mortems:
//   - Processes are NEVER restarted unless restart IS the fault being
//     injected (the L4 lesson: the old scenario modeled the systemd-
//     restart workaround and masked #34). This soak keeps the streamer
//     running across every fault.
//   - The fault sequence is seeded and logged, so any failure is
//     reproducible: PGHS_CHAOS_SEED=<seed> re-runs the same schedule.
//
// Opt-in (needs Docker + the Spilo image):
//
//	PGHS_CHAOS_BIN=/tmp/pghs PGHS_CHAOS_MINUTES=6 \
//	    go test -run TestChaosSoak_RestoreProof -timeout 30m \
//	    ./internal/testkit/topology/
//
// The nightly chaos-soak workflow runs this with a 45-minute budget.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestChaosSoak_RestoreProof(t *testing.T) {
	bin := os.Getenv("PGHS_CHAOS_BIN")
	if bin == "" {
		t.Skip("set PGHS_CHAOS_BIN to the pg_hardstorage binary under test")
	}
	minutes := 6
	if v := os.Getenv("PGHS_CHAOS_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			minutes = n
		}
	}
	seed := time.Now().UnixNano()
	if v := os.Getenv("PGHS_CHAOS_SEED"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			seed = n
		}
	}
	rng := rand.New(rand.NewSource(seed))
	t.Logf("chaos soak: budget=%dm seed=%d (re-run with PGHS_CHAOS_SEED=%d)", minutes, seed, seed)

	scratch, err := os.MkdirTemp("", "chaos-soak-")
	if err != nil {
		t.Fatal(err)
	}
	home := scratch + "/home"
	repoDir := scratch + "/repo"
	repoURL := "file://" + repoDir
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}

	p := newPatroniLocalDocker()
	ctx := context.Background()
	t.Logf("bringing up 3-node Patroni cluster…")
	if err := p.Up(ctx, UpOptions{PGVersion: "17"}); err != nil {
		t.Fatalf("cluster up: %v", err)
	}
	defer p.Down(context.Background())

	var hosts []string
	for _, n := range p.nodes {
		hosts = append(hosts, fmt.Sprintf("127.0.0.1:%d", n.pgPort))
	}
	dsn := fmt.Sprintf("postgres://postgres:%s@%s/postgres?target_session_attrs=primary&sslmode=disable",
		patroniSuperPassword, strings.Join(hosts, ","))

	env := append(os.Environ(), "HOME="+home)
	runBin := func(timeout time.Duration, args ...string) (string, int) {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(cctx, bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			code = -1
		}
		return string(out), code
	}

	// ENCRYPTED repo + first backup + config via init --quick (the #31
	// posture: local KEK, aes-256-gcm, shared DEK across writers).
	if out, code := runBin(3*time.Minute, "init", "--quick",
		"--pg-connection", dsn, "--repo", repoURL, "--encrypt"); code != 0 {
		t.Fatalf("init --quick --encrypt failed (%d):\n%s", code, tail(out, 1500))
	}
	t.Logf("encrypted repo initialised at %s", repoURL)

	// Continuous streamer — NEVER restarted (soak rule #1).
	streamLog, _ := os.Create(scratch + "/stream.log")
	stream := exec.Command(bin, "wal", "stream", "db1",
		"--pg-connection", dsn, "--repo", repoURL, "-o", "text")
	stream.Env = env
	stream.Stdout, stream.Stderr = streamLog, streamLog
	if err := stream.Start(); err != nil {
		t.Fatalf("start streamer: %v", err)
	}
	streamerDone := make(chan error, 1)
	go func() { streamerDone <- stream.Wait() }()
	t.Logf("streamer pid=%d (continuous; restarts are forbidden)", stream.Process.Pid)

	// Constant DB churn: full-page-image-heavy updates on the CURRENT
	// leader (re-resolved every iteration so churn survives failovers).
	churnCtx, stopChurn := context.WithCancel(ctx)
	defer stopChurn()
	go func() {
		i := 0
		for churnCtx.Err() == nil {
			i++
			leader := p.findLeaderName(churnCtx)
			for _, n := range p.nodes {
				out, err := exec.Command("docker", "exec", n.container, "hostname").CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != leader {
					continue
				}
				sql := fmt.Sprintf("create table if not exists churn(id int primary key, pad text); insert into churn select g, repeat('x',300) from generate_series(%d,%d) g on conflict (id) do update set pad=repeat('y',300); checkpoint;", i*500, i*500+499)
				_ = exec.CommandContext(churnCtx, "docker", "exec", n.container,
					"psql", "-U", "postgres", "-h", "127.0.0.1", "-qc", sql).Run()
			}
			select {
			case <-churnCtx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}()

	// Fault rounds until the budget expires.
	faults := []string{"none", "switchover", "pause_leader", "backup_burst"}
	deadline := time.Now().Add(time.Duration(minutes) * time.Minute)
	round := 0
	var faultLog []string
	for time.Now().Before(deadline) {
		round++
		fault := faults[rng.Intn(len(faults))]
		faultLog = append(faultLog, fault)
		t.Logf("round %d: fault=%s", round, fault)

		switch fault {
		case "switchover":
			if leader := p.findLeaderName(ctx); leader != "" {
				if err := p.switchover(ctx, leader); err != nil {
					t.Logf("  switchover request failed (tolerated, cluster may be settling): %v", err)
				}
				// Wait for the demoted node to rejoin — with the #34 fix
				// this happens without touching the streamer. A node that
				// stays stuck would resurface the hang; the final health
				// check below catches it.
				time.Sleep(20 * time.Second)
			}
		case "pause_leader":
			if leader := p.findLeaderName(ctx); leader != "" {
				for _, n := range p.nodes {
					out, err := exec.Command("docker", "exec", n.container, "hostname").CombinedOutput()
					if err == nil && strings.TrimSpace(string(out)) == leader {
						pauseFor := time.Duration(3+rng.Intn(5)) * time.Second
						t.Logf("  pausing leader container %s for %s (GC-stall simulator)", n.container[:12], pauseFor)
						_ = exec.Command("docker", "pause", n.container).Run()
						time.Sleep(pauseFor)
						_ = exec.Command("docker", "unpause", n.container).Run()
					}
				}
			}
		case "backup_burst":
			// Two concurrent backups racing each other + the streamer —
			// the exact #31 shape. One may lose the lease (exit 7): fine.
			done := make(chan int, 2)
			for i := 0; i < 2; i++ {
				go func() {
					_, code := runBin(4*time.Minute, "backup", "db1",
						"--pg-connection", dsn, "--repo", repoURL)
					done <- code
				}()
			}
			<-done
			<-done
		}

		// Every round also takes one scheduled-style backup.
		if out, code := runBin(4*time.Minute, "backup", "db1",
			"--pg-connection", dsn, "--repo", repoURL); code != 0 && code != 7 {
			t.Fatalf("round %d: backup failed with unexpected code %d (7=lease-conflict is tolerated):\n%s", round, code, tail(out, 1200))
		}
		time.Sleep(time.Duration(3+rng.Intn(5)) * time.Second)
	}
	stopChurn()
	t.Logf("fault schedule (%d rounds): %s", round, strings.Join(faultLog, ","))

	// The streamer must still be ALIVE after every fault (a crash or a
	// silent exit is a soak failure in itself).
	select {
	case err := <-streamerDone:
		logs, _ := os.ReadFile(scratch + "/stream.log")
		t.Fatalf("streamer exited mid-soak: %v\n--- stream log tail ---\n%s", err, tail(string(logs), 2500))
	default:
	}

	// Graceful stop (single SIGINT — exercises the graceful-drain path).
	_ = stream.Process.Signal(syscall.SIGINT)
	select {
	case <-streamerDone:
	case <-time.After(30 * time.Second):
		_ = stream.Process.Kill()
		<-streamerDone
	}

	// ---- RESTORE-PROOF GATE -------------------------------------------
	// 1. Exactly one shared-DEK object (the #31 invariant).
	dekObjs, _ := os.ReadDir(repoDir + "/keys/shared-dek")
	if len(dekObjs) != 1 {
		t.Errorf("shared-DEK objects = %d, want exactly 1 (divergent DEKs = #31 class)", len(dekObjs))
	}

	// 2. Every committed backup must verify AND restore.
	out, code := runBin(2*time.Minute, "list", "db1", "--repo", repoURL, "-o", "text")
	if code != 0 {
		t.Fatalf("list failed (%d):\n%s", code, out)
	}
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && strings.HasPrefix(f[0], "db1.full.") {
			ids = append(ids, f[0])
		}
	}
	if len(ids) < 2 {
		t.Fatalf("soak committed only %d backups — not a meaningful proof (list output:\n%s)", len(ids), out)
	}
	t.Logf("restore-proof gate: %d backups to verify + restore", len(ids))
	for _, id := range ids {
		if out, code := runBin(5*time.Minute, "verify", "db1", id, "--repo", repoURL, "--full"); code != 0 {
			t.Errorf("PROOF FAILED: verify --full %s exited %d:\n%s", id, code, tail(out, 1200))
			continue
		}
		target := scratch + "/restore-" + id[len(id)-4:]
		if out, code := runBin(5*time.Minute, "restore", "db1", id, "--repo", repoURL,
			"--target", target, "--verify=skip"); code != 0 {
			t.Errorf("PROOF FAILED: restore %s exited %d:\n%s", id, code, tail(out, 1200))
		}
		_ = os.RemoveAll(target)
	}

	// 3. The WAL lineage must be gap-free (slot continuity survived
	//    every switchover/pause without a streamer restart).
	if out, code := runBin(2*time.Minute, "wal", "audit", "db1", "--repo", repoURL, "-o", "text"); code != 0 {
		t.Errorf("PROOF FAILED: wal audit exited %d (gap or lineage fault):\n%s", code, tail(out, 1500))
	}

	if !t.Failed() {
		t.Logf("✓ chaos soak passed: %d rounds, %d backups all verified+restored, WAL gap-free, single shared DEK (seed=%d)", round, len(ids), seed)
	}
}
