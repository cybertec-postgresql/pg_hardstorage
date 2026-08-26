// This soak needs Docker, the Spilo image and minutes of wall-clock
// budget, so it is excluded from the default suite by BUILD TAG rather
// than by a runtime skip.
//
// The distinction matters and was got wrong once. Making the soak
// unable to skip (commit 28dd8cd, "a soak must not report PASS without
// running") removed its t.Skip and had it build a binary instead — at
// which point `go test ./internal/...` started running a 3-node Patroni
// chaos soak, and the -race suite hit its 30-minute timeout in this
// package. Both properties are wanted: not in the default suite, AND
// unable to silently skip once you ask for it. A build tag gives the
// first at compile time; chaosBinary's fatal-on-unusable-binary gives
// the second.
//
//go:build chaos

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
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/pgboot"
)

func TestChaosSoak_RestoreProof(t *testing.T) {
	bin := chaosBinary(t)
	// Fault-window sizing. PGHS_CHAOS_MINUTES is the explicit
	// override; otherwise size from the test binary's own deadline so
	// the verify+boot gate that follows the window always has room.
	// The arithmetic that matters: fast hardware turns fault-minutes
	// into backups at ~4.5/min, and the gate verifies at ~5/min — so
	// the window must stay well under half the remaining budget or
	// the gate ends with the honest-but-red PROOF INCOMPLETE. Two
	// hosts proved this the same week: a local box (45m window → 206
	// backups → 69 verified in budget) and, after a runner-hardware
	// upgrade, CI itself (45m → 203 → 106). The empirical record —
	// not theory — sets the divisor: the gate's observed verify
	// budget is ~22m whatever the -timeout, and the one fully-green
	// shape was a 15m window (81 backups, all verified+booted). An
	// eighth of the budget, capped at 20m, stays inside that record
	// on both hosts.
	minutes := 6
	if v := os.Getenv("PGHS_CHAOS_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			minutes = n
		}
	} else if deadline, ok := t.Deadline(); ok {
		if m := int(time.Until(deadline).Minutes() / 8); m > 0 {
			minutes = m
			if minutes > 20 {
				minutes = 20
			}
			t.Logf("chaos soak: fault window auto-sized to %dm from the %s test budget "+
				"(set PGHS_CHAOS_MINUTES to override)", minutes, time.Until(deadline).Round(time.Minute))
		}
	}
	seed := time.Now().UnixNano()
	if v := os.Getenv("PGHS_CHAOS_SEED"); v != "" {
		// Refuse an unusable seed rather than quietly running a
		// different one.
		//
		// The parse error used to be discarded, so an out-of-range or
		// malformed PGHS_CHAOS_SEED left the auto-selected seed in
		// place and the soak ran a DIFFERENT fault ordering than the
		// operator asked for. This file promises the opposite —
		// "PGHS_CHAOS_SEED=<seed> re-runs the same schedule" — and the
		// promise is the whole reason the seed is printed on every run:
		// somebody chasing a chaos failure sets it to reproduce, gets
		// an unrelated schedule, concludes the failure is not
		// reproducible, and drops a real bug.
		//
		// Caught by passing a seed drawn as an unsigned 64-bit value:
		// 17327145514990876780 is above math.MaxInt64, so ParseInt
		// failed and the run silently used its own seed.
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("PGHS_CHAOS_SEED=%q is not usable as a seed: %v\n"+
				"It must be a signed 64-bit integer (%d..%d) — note that an unsigned "+
				"64-bit value from /dev/urandom can exceed the maximum. Refusing rather "+
				"than running a different schedule than you asked for.",
				v, err, int64(math.MinInt64), int64(math.MaxInt64))
		}
		seed = n
	}
	rng := rand.New(rand.NewSource(seed))
	t.Logf("chaos soak: budget=%dm seed=%d (re-run with PGHS_CHAOS_SEED=%d)", minutes, seed, seed)

	scratch, err := os.MkdirTemp("", "chaos-soak-")
	if err == nil {
		// Ten multi-GB scratch trees from earlier runs were found
		// leaked in /tmp: nothing ever removed them. Cleanup runs LAST
		// (registered first), after pgboot's chown-backs.
		t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	}
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

	// HOME alone is not enough: paths.Resolve prefers the XDG
	// variables when set, and GitHub's runners set them globally — so
	// the agent's keyring (kek.bin) landed OUTSIDE the soak's
	// controlled home on CI and the boot-proof prep found nothing to
	// decrypt with. Blank them all, the same lesson newReadWorld
	// already carries.
	env := append(os.Environ(), "HOME="+home,
		"XDG_CONFIG_HOME=", "XDG_DATA_HOME=", "XDG_CACHE_HOME=",
		"XDG_STATE_HOME=", "XDG_RUNTIME_DIR=",
		"PG_HARDSTORAGE_ROOT=", "PG_HARDSTORAGE_CONFIG_DIR=")
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
	var churnWrites atomic.Int64
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
				// Unix socket, not -h 127.0.0.1: Spilo's host hba is
				// md5, and psql without a password exits 2 — which the
				// original `_ = Run()` swallowed, so the churn workload
				// had NEVER written a row in any soak. Every content-
				// free assertion (gap-free lineage, DEK count, restore
				// exit codes) passed over an empty database for months;
				// the first boot-proof that SELECTed the data exposed
				// it. Local socket connections are trust in Spilo, and
				// PGPASSWORD is the belt to the braces.
				if err := exec.CommandContext(churnCtx, "docker", "exec",
					"-e", "PGPASSWORD="+patroniSuperPassword, n.container,
					"psql", "-U", "postgres", "-qc", sql).Run(); err == nil {
					churnWrites.Add(1)
				}
			}
			select {
			case <-churnCtx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}()

	// Fault rounds until the budget expires.
	faults := []string{"none", "switchover", "pause_leader", "backup_burst", "dcs_outage", "compound_storm", "janitor_sweep"}
	deadline := time.Now().Add(time.Duration(minutes) * time.Minute)
	round := 0
	roundBackupsOK := 0
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
		case "janitor_sweep":
			// Retention AS chaos. Every retention proof so far ran the
			// janitors on a QUIET repo; the dedup-vs-GC commit gates
			// (adopted-chunk re-stat in both the backup runner and the
			// walsink) were proven at unit level. Here the real
			// janitors race the real pipeline: gc --apply and wal
			// prune --apply while the streamer commits segments,
			// round backups run, and whatever fault the NEXT round
			// brings lands on top. Two outcomes are correct: the
			// janitor completes (deletions judged by the end gate —
			// every backup must still verify, restore, and boot), or
			// gc refuses because a backup lease is live (that refusal
			// IS the dedup-vs-GC protection working; log it). What
			// must never happen is what the end gate exists to catch:
			// a backup or segment that verifies as present but lost a
			// chunk to a sweep the gates should have blocked.
			gcOut, gcCode := runBin(4*time.Minute, "repo", "gc", "--repo", repoURL,
				"--apply")
			switch {
			case gcCode == 0:
				t.Logf("  janitor: repo gc --apply completed mid-storm")
			case strings.Contains(gcOut, "live_backup_lease"):
				t.Logf("  janitor: gc refused under a live backup lease — the dedup-vs-GC guard, working")
			default:
				t.Errorf("janitor_sweep round %d: repo gc failed unexpectedly (%d):\n%s",
					round, gcCode, tail(gcOut, 800))
			}
			if pOut, pCode := runBin(4*time.Minute, "wal", "prune", "db1", "--repo", repoURL,
				"--apply"); pCode != 0 {
				t.Errorf("janitor_sweep round %d: wal prune failed (%d):\n%s",
					round, pCode, tail(pOut, 800))
			} else {
				t.Logf("  janitor: wal prune --apply completed mid-storm")
			}
		case "compound_storm":
			// Faults do not strike alone in real incidents. The
			// nastiest realistic stack: the DCS goes unreachable AND
			// the leader dies hard, together. The survivors must hold
			// until etcd returns, elect among themselves with the old
			// leader entirely absent (no demoted-but-alive node to
			// mislead anything), and the killed node later rejoins as
			// a replica. The streamer's whole gauntlet in one round:
			// dead peer, no primary, re-election, recreated slot.
			if p.etcdContainer != "" {
				if leader := p.findLeaderName(ctx); leader != "" {
					var leaderContainer string
					for _, n := range p.nodes {
						out, err := exec.Command("docker", "exec", n.container, "hostname").CombinedOutput()
						if err == nil && strings.TrimSpace(string(out)) == leader {
							leaderContainer = n.container
						}
					}
					if leaderContainer != "" {
						t.Logf("  COMPOUND STORM: pausing etcd AND killing leader %s", leaderContainer[:12])
						_ = exec.Command("docker", "pause", p.etcdContainer).Run()
						_ = exec.Command("docker", "kill", leaderContainer).Run()
						time.Sleep(time.Duration(31+rng.Intn(10)) * time.Second) // past the TTL, DCS still dark
						_ = exec.Command("docker", "unpause", p.etcdContainer).Run()
						// Election among survivors, then the dead node
						// rejoins as a replica (pg_rewind/basebackup —
						// Spilo handles it; the rejoin test pins it).
						time.Sleep(15 * time.Second)
						_ = exec.Command("docker", "start", leaderContainer).Run()
						time.Sleep(15 * time.Second)
					}
				}
			}
		case "dcs_outage":
			// The DCS is the failure mode the OTHER faults never
			// touch: pause etcd for 10-45s, straddling Patroni's 30s
			// TTL. Below it the cluster rides the outage out; above
			// it the leader SELF-DEMOTES (DCS-loss safety — it cannot
			// prove it still holds the lock), every node is a replica
			// until etcd returns and re-election runs. The streamer
			// must refuse the demoted-but-alive node
			// (wal.source_in_recovery is retryable by design), pick
			// up the re-elected leader, and leave an archive the end
			// gate can still prove gap-free.
			if p.etcdContainer != "" {
				pauseFor := time.Duration(10+rng.Intn(36)) * time.Second
				t.Logf("  pausing etcd (DCS) for %s — Patroni ttl is 30s, so %s",
					pauseFor, map[bool]string{true: "expect leader self-demotion + re-election", false: "cluster should ride it out"}[pauseFor > 30*time.Second])
				_ = exec.Command("docker", "pause", p.etcdContainer).Run()
				time.Sleep(pauseFor)
				_ = exec.Command("docker", "unpause", p.etcdContainer).Run()
				// Give the DCS session + election a beat to settle
				// before the next fault lands on top of it.
				time.Sleep(10 * time.Second)
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

		// Every round also takes one scheduled-style backup. Two
		// failure codes are CORRECT behaviour under this schedule, not
		// findings: 7 (lease conflict — a burst backup still holds the
		// lease) and 8 (unreachable — a dcs_outage demotion storm can
		// leave the cluster PRIMARY-LESS for tens of seconds while
		// re-election converges, and refusing loudly to back up a
		// cluster with no primary is exactly right; CI hit this on the
		// fault's first outing, round 9, mid-election after a 34s DCS
		// pause compounded by a leader pause). The success-floor
		// assertion after the loop keeps this tolerance from masking a
		// backup path that is ALWAYS failing.
		if out, code := runBin(4*time.Minute, "backup", "db1",
			"--pg-connection", dsn, "--repo", repoURL); code == 0 {
			roundBackupsOK++
		} else if code != 7 && code != 8 {
			t.Fatalf("round %d: backup failed with unexpected code %d (7=lease-conflict and 8=no-primary/unreachable are tolerated):\n%s", round, code, tail(out, 1200))
		}
		time.Sleep(time.Duration(3+rng.Intn(5)) * time.Second)
	}
	stopChurn()
	// The tolerance above must not hide a systemically broken backup
	// path: across a whole schedule, at least a third of the rounds
	// must have produced a successful backup — faults are transient,
	// and even demotion storms resolve within a round or two.
	if roundBackupsOK < round/3 {
		t.Fatalf("only %d of %d rounds produced a successful backup — the tolerated "+
			"failure codes are masking a backup path that is failing SYSTEMICALLY, "+
			"not transiently", roundBackupsOK, round)
	}
	t.Logf("round backups: %d/%d succeeded", roundBackupsOK, round)
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
	// The gate is O(backups) and each iteration is a full verify plus a
	// real restore — tens of seconds each. It is the longest phase of
	// the soak by far, and it used to print ONE line and then nothing
	// until it finished.
	//
	// That silence cost real time: a 20-minute soak produced 80
	// backups, the gate could not finish them inside the test timeout,
	// and the run died with `panic: test timed out` naming only the
	// test. Telling "stuck" from "still working" needed the goroutine
	// dump, where the exec watchdog showed the current subprocess had
	// been running two minutes — i.e. fine, just slow. A nightly job
	// with a longer fault budget produces MORE backups and is more
	// likely to hit this, not less.
	//
	// So: report progress, and own the deadline rather than letting the
	// test timeout own it. Running out of budget now says how far it
	// got, which is actionable; a goroutine dump is not.
	// t.Deadline(), NOT t.Context().Deadline(): the test context
	// carries no deadline in Go's testing package, so this call always
	// reported ok=false and the gate silently ran on the 30-minute
	// fallback below — a hardcoded ~22m verify budget (after the
	// reserve) on every host, whatever -timeout said. That constant
	// was measured three times across two machines before its origin
	// was found: every PROOF INCOMPLETE of the week traces here, and
	// the -timeout 150m the CI lane grants was never reaching the
	// verify loop.
	gateDeadline, hasGateDeadline := t.Deadline()
	if !hasGateDeadline {
		gateDeadline = time.Now().Add(30 * time.Minute)
	}
	// Leave room for the WAL-audit and shared-DEK checks below, which
	// are the other half of the proof and must not be skipped — and for
	// the two sampled BOOT proofs, which cost ~90s each.
	gateDeadline = gateDeadline.Add(-8 * time.Minute)

	t.Logf("restore-proof gate: %d backups to verify + restore (budget until %s)",
		len(ids), gateDeadline.Format(time.TimeOnly))
	gateStart := time.Now()
	proved := 0

	// The workload is part of the proof. A soak whose churn writer
	// silently failed proves recovery of an EMPTY database — every
	// object-level assertion still passes, which is precisely how this
	// went unnoticed until a boot-proof SELECTed the rows.
	if got := churnWrites.Load(); got == 0 {
		t.Fatalf("the churn workload wrote NOTHING for the entire fault window — the soak " +
			"exercised backups of an idle database and every downstream assertion is " +
			"vacuous")
	}
	t.Logf("churn workload: %d successful write batches", churnWrites.Load())

	// Boot-proof sampling. The per-backup loop below proves objects:
	// verify --full walks every chunk hash, restore materialises every
	// file. Neither asks PostgreSQL, and the retention campaign showed
	// exactly what that misses — a repo can hold a kept backup whose
	// every object checks out while its replay window has a hole, and
	// only a boot notices. Booting all N backups would blow the budget,
	// so the OLDEST and NEWEST are booted end-to-end (--to-latest,
	// promote, query the churn table): oldest exercises the longest
	// replay over the most faults, newest exercises the freshest
	// lineage. Object-level proof stays exhaustive; PG-accepts proof is
	// sampled.
	sampled := map[int]bool{0: true, len(ids) - 1: true}
	t.Logf("boot-proof samples: ids[0]=%s ids[%d]=%s", ids[0], len(ids)-1, ids[len(ids)-1])
	var bootKeyring string
	bootPrep := func() bool {
		if bootKeyring != "" {
			return true
		}
		// The boot container (uid 999) must traverse into the repo and
		// read the KEK. scratch is MkdirTemp-0700; open it up, expose
		// the repo read-only-wide, and hand the container its OWN 0600
		// copy of the KEK — the keystore refuses group/world-readable
		// key files, so a chmod of the original cannot work.
		if err := os.Chmod(scratch, 0o755); err != nil {
			t.Errorf("boot-proof prep: chmod scratch: %v", err)
			return false
		}
		if out, err := exec.Command("chmod", "-R", "a+rX", repoDir).CombinedOutput(); err != nil {
			t.Errorf("boot-proof prep: chmod repo: %v\n%s", err, out)
			return false
		}
		var kek string
		_ = filepath.Walk(home, func(pth string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && info.Name() == "kek.bin" {
				kek = pth
			}
			return nil
		})
		if kek == "" {
			t.Error("boot-proof prep: no kek.bin under HOME — the soak repo is encrypted and " +
				"the boots cannot decrypt WAL without it")
			return false
		}
		dir := scratch + "/boot-keyring"
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Errorf("boot-proof prep: %v", err)
			return false
		}
		raw, rerr := os.ReadFile(kek)
		if rerr != nil {
			t.Errorf("boot-proof prep: read kek: %v", rerr)
			return false
		}
		if werr := os.WriteFile(dir+"/kek.bin", raw, 0o600); werr != nil {
			t.Errorf("boot-proof prep: write kek copy: %v", werr)
			return false
		}
		pgboot.ChownAll(t, context.Background(), dir, "999:999")
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			pgboot.ChownAll(t, cctx, dir, fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
		})
		bootKeyring = dir
		return true
	}

	for i, id := range ids {
		if time.Now().After(gateDeadline) {
			t.Errorf("PROOF INCOMPLETE: verified+restored %d of %d backups in %s before "+
				"running out of budget.\n\n"+
				"This is not a product failure and not a hang — it is the gate being "+
				"unable to finish. Either raise -timeout, lower PGHS_CHAOS_MINUTES (fewer "+
				"backup_burst rounds means fewer backups), or accept a sampled proof. What "+
				"it must never do is die at the test timeout with no count, which is "+
				"indistinguishable from a stuck restore.",
				proved, len(ids), time.Since(gateStart).Truncate(time.Second))
			break
		}
		if out, code := runBin(5*time.Minute, "verify", "db1", id, "--repo", repoURL, "--full"); code != 0 {
			// A child killed by the gate's own deadline (ctx cancel →
			// SIGKILL → exit -1 right as the budget check above trips
			// on the next loop) is BUDGET, not corruption — reporting
			// it as PROOF FAILED sent one investigation chasing a
			// phantom verify failure that was the deadline's own
			// mechanism.
			if time.Now().After(gateDeadline) {
				t.Logf("verify --full %s cut by the gate deadline mid-flight (exit %d) — counted as unverified, not failed", id, code)
				continue
			}
			t.Errorf("PROOF FAILED: verify --full %s exited %d:\n%s", id, code, tail(out, 1200))
			continue
		}
		target := scratch + "/restore-" + id[len(id)-4:]
		if out, code := runBin(5*time.Minute, "restore", "db1", id, "--repo", repoURL,
			"--target", target, "--verify=skip"); code != 0 {
			t.Errorf("PROOF FAILED: restore %s exited %d:\n%s", id, code, tail(out, 1200))
		}
		_ = os.RemoveAll(target)
		proved++

		if sampled[i] && bootPrep() {
			// Index in the dir name, not an id suffix: two sampled ids
			// sharing a suffix once reused one target, and the second
			// restore ran into the first boot's uid-999 residue —
			// contaminating exactly the forensic evidence a failed
			// sample exists to provide.
			bootTarget := fmt.Sprintf("%s/boot-%02d", scratch, i)
			if out, code := runBin(6*time.Minute, "restore", "db1", id, "--repo", repoURL,
				"--target", bootTarget, "--to-latest", "--to-action", "promote"); code != 0 {
				if strings.Contains(out, "target_in_wal_gap") {
					// The product REFUSED, loudly and typed: this backup
					// predates a recorded unarchivable window (e.g. it
					// was taken before the streamer first started). A
					// loud refusal is the correct outcome — the failure
					// this gate hunts is SILENCE.
					t.Logf("  BOOT-PROOF: %s refused with target_in_wal_gap (recorded "+
						"pre-stream window) — loud refusal is the correct outcome", id)
				} else {
					t.Errorf("BOOT-PROOF FAILED: restore --to-latest %s exited %d:\n%s",
						id, code, tail(out, 1200))
				}
			} else {
				bctx := context.Background()
				b := pgboot.Boot(t, bctx, "postgres:17", bootTarget, []string{
					bin + ":" + bin + ":ro",
					repoDir + ":" + repoDir + ":ro",
					bootKeyring + ":" + bootKeyring + ":ro",
				}, []string{"PG_HARDSTORAGE_KEYRING_DIR=" + bootKeyring})
				if werr := b.WaitPromoted(bctx, 4*time.Minute); werr != nil {
					t.Errorf("BOOT-PROOF FAILED: %s: %v\nboot log:\n%s",
						id, werr, tail(b.Logs(bctx), 2500))
				} else if rows, qerr := b.Query(bctx, "SELECT count(*) FROM churn"); qerr != nil || rows == "0" || rows == "" {
					t.Errorf("BOOT-PROOF FAILED: %s promoted but the churn table is empty or "+
						"unreadable (rows=%q err=%v) — the workload the soak spent its fault "+
						"window writing did not come back.\nboot log:\n%s",
						id, rows, qerr, tail(b.Logs(bctx), 2500))
				} else {
					t.Logf("  BOOT-PROOF ok: %s booted, promoted, churn rows=%s", id, rows)
				}
				// Free the container + datadir now rather than at test
				// end — two live postgres containers during the rest of
				// the gate would skew its timing.
				_, _ = pgboot.Docker(bctx, "rm", "-f", b.Name)
			}
		}
		// Progress every few backups: enough to see it moving, not so
		// much that the log becomes noise.
		if (i+1)%5 == 0 || i+1 == len(ids) {
			t.Logf("  restore-proof: %d/%d verified+restored (%s elapsed)",
				i+1, len(ids), time.Since(gateStart).Truncate(time.Second))
		}
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

// chaosBinary resolves the pg_hardstorage binary this soak drives,
// BUILDING it if necessary.
//
// It used to skip when PGHS_CHAOS_BIN was unset. A skip reports PASS,
// and a harness that greps for pass/fail — including the soak campaign
// this test was written for — then records a chaos phase that never
// ran as a chaos phase that succeeded. The 4-hour campaign of
// 2026-08-05 did exactly that: the phase "passed" in two seconds.
//
// A test that needs an artifact it can produce should produce it. The
// only remaining skip is the genuinely-unavailable case (no Go
// toolchain to build with), and that one is loud.
func chaosBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("PGHS_CHAOS_BIN"); bin != "" {
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("PGHS_CHAOS_BIN=%s is not usable: %v — an explicitly-set binary that "+
				"does not exist is a configuration error, not a reason to skip", bin, err)
		}
		return bin
	}

	// ALWAYS build from the working tree.
	//
	// This used to prefer an existing bin/pg_hardstorage. On CI that is
	// harmless — a fresh checkout has none, so it builds — but on a
	// developer machine bin/ holds whatever was last built, and the
	// soak then spends hours proving things about code that is no
	// longer in the tree. A sibling test was caught doing exactly that:
	// it ran a binary from the previous day and reported on a fix it
	// did not contain.
	//
	// PGHS_CHAOS_BIN above remains the explicit override, because
	// asking for a specific binary is a deliberate act. Silently
	// picking up a stale one is not.
	root := repoRootForChaos(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("no Go toolchain to build the binary under test: %v", err)
	}
	out := filepath.Join(t.TempDir(), "pg_hardstorage")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/pg_hardstorage")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the binary under test failed: %v\n%s", err, b)
	}
	t.Logf("chaos soak: built %s", out)
	return out
}

func repoRootForChaos(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", ".."))
}
