// pitr_across_failover_test.go — the invariant the whole campaign is
// about: WAL written AFTER a promotion must be recoverable.
//
// Everything else proves a piece. The chaos soak proves backups restore
// to files; the gap fixes prove holes get reported; the history capture
// proves one file lands. None of them answers the operator's actual
// question, which is whether a recovery that crosses a failover can
// replay right through to a row committed on the new primary.
//
// Answering it needs the archive to be complete and SERVABLE across the
// timeline change, which is three separate things:
//
//   - every WAL segment from the backup's start through the last commit
//     on the NEW timeline is in the repository;
//   - `wal fetch` — the exact command PG's restore_command runs —
//     returns each of them;
//   - `<tli>.history` is fetchable under the eight-hex-digit archive
//     name PG asks for, which is not the decimal name it is stored
//     under.
//
// That last one is the join that used to have no producer at all. With
// recovery_target_timeline='latest' PG does not fail when it is
// missing: it follows the highest timeline it can resolve history for,
// recovers along the pre-failover timeline, and reports success while
// silently dropping everything written after the promotion.
//
// This test deliberately does NOT boot PostgreSQL on the restored
// directory. The cluster runs Spilo PG 17 and the host has PG 16, so a
// boot would soft-skip — and a skip reported as a pass is the failure
// mode this repository has been bitten by more than once. Fetching
// every segment PG would ask for is a real assertion that runs
// everywhere.

//go:build integration && patroni

package topology_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/topology"
)

// TestPITR_AcrossAFailover_ArchiveServesEverySegment is the proof.
func TestPITR_AcrossAFailover_ArchiveServesEverySegment(t *testing.T) {
	topo, err := topology.Build("patroni-local-docker")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
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

	// A LEADER-AWARE DSN: every node, target_session_attrs=primary. This
	// is the shape that survives a failover, and the shape the previous
	// test proved is required — a single-host DSN reconnects to the
	// demoted node.
	leaderAware := leaderAwareDSN(cluster.NodeDSNs())
	t.Logf("streaming with a leader-aware DSN across %d nodes", len(cluster.NodeDSNs()))

	bin := buildProductBinary(t)
	// World-traversable: proof 4 boots a container (uid 999) whose
	// restore_command must read this repo. See mkSharedDir.
	repoDir := mkSharedDir(t, "pghs-pitr-repo-", 0o755)
	repoURL := "file://" + repoDir
	home := t.TempDir()
	// The keyring lives in a world-readable shared dir because proof 4
	// mounts it into the boot container: the WAL is ENCRYPTED (init
	// --quick generates a local KEK), so the restore_command running
	// inside recovery needs key access — the same requirement an
	// operator's DR runbook has. Without it, wal fetch refuses with
	// wal.fetch.decrypt_unavailable, which is the typed error doing its
	// job, not a fixture fault.
	keyringDir := mkSharedDir(t, "pghs-pitr-keyring-", 0o755)
	env := append(os.Environ(), "HOME="+home,
		"PG_HARDSTORAGE_KEYRING_DIR="+keyringDir)

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

	// Stream continuously, following the leader.
	streamCtx, stopStream := context.WithTimeout(ctx, 15*time.Minute)
	defer stopStream()
	logPath := filepath.Join(t.TempDir(), "stream.log")
	streamLog, _ := os.Create(logPath)
	defer streamLog.Close()
	stream := exec.CommandContext(streamCtx, bin, "wal", "stream", "db1",
		"--pg-connection", leaderAware, "--repo", repoURL, "-o", "text")
	stream.Env = env
	stream.Stdout, stream.Stderr = streamLog, streamLog
	if serr := stream.Start(); serr != nil {
		t.Fatalf("start streamer: %v", serr)
	}
	streamDone := make(chan error, 1)
	go func() { streamDone <- stream.Wait() }()
	// Idempotent, because the body reaps the streamer too and
	// streamDone carries exactly one value: a second receive would
	// block forever and hang the test in teardown, with the cluster
	// still up. (It did — that is why this is a sync.Once.)
	var reapOnce sync.Once
	reapStreamer := func() {
		reapOnce.Do(func() {
			stopStream()
			<-streamDone
		})
	}
	defer reapStreamer()
	time.Sleep(10 * time.Second)

	// Marker A, on timeline 1.
	execSQL(t, ctx, topo, `CREATE TABLE failover_proof(id int primary key, note text)`)
	execSQL(t, ctx, topo, `INSERT INTO failover_proof VALUES (1, 'before-failover')`)
	execSQL(t, ctx, topo, `SELECT pg_switch_wal()`)
	tliBefore := currentTimeline(t, ctx, topo)
	t.Logf("marker A committed on timeline %d", tliBefore)

	// Promote.
	_ = switchover(t, ctx, topo, topo.ConnString())
	t.Logf("switchover complete")

	// Marker B, on the NEW timeline. This is the row that must survive.
	execSQL(t, ctx, topo, `INSERT INTO failover_proof VALUES (2, 'AFTER-failover')`)
	execSQL(t, ctx, topo, `SELECT pg_switch_wal()`)
	// A second switch so the segment holding marker B is definitely
	// closed and eligible for archiving, not still being written.
	execSQL(t, ctx, topo, `SELECT pg_switch_wal()`)
	lsnB := queryVal(t, ctx, topo, `SELECT pg_current_wal_lsn()::text`)
	tliAfter := currentTimeline(t, ctx, topo)
	t.Logf("marker B committed on timeline %d at LSN %s", tliAfter, lsnB)

	if tliAfter <= tliBefore {
		t.Fatalf("the timeline did not advance (%d -> %d); the switchover did not promote "+
			"a new leader and this test is not testing what it claims", tliBefore, tliAfter)
	}

	// Let the streamer catch up to the post-failover WAL.
	waitForArchivedTimeline(t, ctx, repoDir, tliAfter, 3*time.Minute)

	reapStreamer()

	// --- Proof 1: the archive is gap-free ACROSS the timeline change.
	//
	// This assertion only became meaningful once findGaps stopped
	// skipping timeline transitions; before that it was structurally
	// blind at exactly this boundary.
	if out, code := runBin(2*time.Minute, "wal", "audit", "db1", "--repo", repoURL, "-o", "json"); code != 0 {
		t.Errorf("`wal audit` reports the archive is not gap-free after the failover "+
			"(exit %d):\n%s", code, lastLines(out, 2000))
	} else {
		t.Logf("proof 1: archive gap-free across timelines %d -> %d", tliBefore, tliAfter)
	}

	// --- Proof 2: the timeline-history file is fetchable under the
	// name PG actually asks for.
	//
	// PG requests eight hex digits (`00000002.history`); the follower
	// store holds it under the decimal timeline. If those two disagree
	// the file is written and never found, which behaves exactly like
	// never writing it.
	histName := fmt.Sprintf("%08X.history", tliAfter)
	histTarget := filepath.Join(t.TempDir(), histName)
	if out, code := runBin(time.Minute, "wal", "fetch", "db1", histName, histTarget,
		"--repo", repoURL); code != 0 {
		t.Errorf("`wal fetch db1 %s` failed (exit %d):\n%s\n\n"+
			"This is the file PG needs to walk past the failover. With "+
			"recovery_target_timeline='latest' its absence does not fail recovery — PG "+
			"follows the highest timeline it CAN resolve, recovers along timeline %d, and "+
			"reports success having dropped everything written after the promotion.",
			histName, code, lastLines(out, 1200), tliBefore)
	} else {
		body, _ := os.ReadFile(histTarget)
		t.Logf("proof 2: %s fetchable (%d bytes): %q",
			histName, len(body), strings.TrimSpace(string(body)))
	}

	// --- Proof 3: every archived segment is fetchable, on BOTH
	// timelines.
	//
	// restore_command runs `wal fetch` once per segment. A segment
	// present in the repository but unfetchable — a lost chunk, a DEK
	// the fetch path cannot resolve, a name the fetch side parses
	// differently — fails recovery at exactly the point the operator
	// needs it most.
	segs := archivedSegments(t, repoDir)
	if len(segs) == 0 {
		t.Fatal("no segments archived at all; the streamer never committed anything and " +
			"every assertion here would be vacuous")
	}
	var onOld, onNew int
	for _, s := range segs {
		if s.tli == tliAfter {
			onNew++
		} else {
			onOld++
		}
		target := filepath.Join(t.TempDir(), s.name)
		if out, code := runBin(time.Minute, "wal", "fetch", "db1", s.name, target,
			"--repo", repoURL); code != 0 {
			t.Errorf("`wal fetch db1 %s` failed (exit %d) — the segment is in the "+
				"repository but restore_command cannot get it back:\n%s",
				s.name, code, lastLines(out, 800))
		}
	}
	if onNew == 0 {
		t.Errorf("no segments archived on the POST-failover timeline %d.\n\n"+
			"Marker B was committed there. If nothing from that timeline reached the "+
			"archive, a recovery cannot replay past the promotion no matter what the "+
			"history file says — which is the data-loss shape this whole test exists for.",
			tliAfter)
	}
	t.Logf("proof 3: %d segment(s) fetchable — %d on timeline %d, %d on timeline %d",
		len(segs), onOld, tliBefore, onNew, tliAfter)

	// --- Proof 4: a real PostgreSQL replays it. Everything above says
	// the archive is complete and servable; this says PG AGREES —
	// restore the pre-failover backup, recover with --to-latest, and
	// the server must cross 2.history onto timeline 2 and serve marker
	// B, the row that only ever existed after the promotion. This is
	// the assertion the whole campaign exists for.
	//
	// The Spilo cluster is PG 17, so the boot container is postgres:17
	// — same major, version-exact.
	if out, err := exec.CommandContext(ctx, "chmod", "-R", "a+rX", repoDir).CombinedOutput(); err != nil {
		t.Fatalf("chmod repo: %v\n%s", err, out)
	}
	targetRoot := mkSharedDir(t, "pghs-pitr-target-", 0o755)
	target := filepath.Join(targetRoot, "restored")
	if out, code := runBin(6*time.Minute, "restore", "db1", "latest",
		"--repo", repoURL, "--target", target,
		"--to-latest", "--to-action", "promote"); code != 0 {
		t.Fatalf("restore --to-latest failed (%d):\n%s", code, lastLines(out, 1500))
	}
	// The boot container needs the KEK, and the keystore's mode gate
	// dictates HOW: kek.bin must be exactly 0600 (a chmod a+r "fix"
	// here cost a debugging session — the loader refuses 0644 by
	// design), which for uid 999 means a copy OWNED by 999. This is
	// the same provisioning a real recovery host needs, expressed in
	// docker: the key arrives as the service user's own 0600 file.
	bootKeyring := mkSharedDir(t, "pghs-pitr-bootkeyring-", 0o755)
	kek, rerr := os.ReadFile(filepath.Join(keyringDir, "kek.bin"))
	if rerr != nil {
		t.Fatalf("read kek.bin: %v", rerr)
	}
	if werr := os.WriteFile(filepath.Join(bootKeyring, "kek.bin"), kek, 0o600); werr != nil {
		t.Fatal(werr)
	}
	chownAll(t, ctx, bootKeyring, "999:999")
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		me := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
		chownAll(t, cctx, bootKeyring, me)
	})
	boot := bootRestoredDatadir(t, ctx, "postgres:17", target, []string{
		bin + ":" + bin + ":ro",
		repoDir + ":" + repoDir + ":ro",
		bootKeyring + ":" + bootKeyring + ":ro",
	}, []string{
		"PG_HARDSTORAGE_KEYRING_DIR=" + bootKeyring,
	})
	boot.AwaitPromoted(t, ctx, 4*time.Minute)

	rows, qerr := boot.Query(ctx, `SELECT note FROM failover_proof ORDER BY id`)
	if qerr != nil {
		t.Fatalf("query after promotion: %v\n%s", qerr, boot.Logs(ctx))
	}
	if !strings.Contains(rows, "before-failover") {
		t.Errorf("proof 4: marker A missing — the restore itself is broken: %q", rows)
	}
	if !strings.Contains(rows, "AFTER-failover") {
		t.Errorf("proof 4: marker B missing.\n\nThe server promoted, but the row written "+
			"AFTER the failover never arrived: PostgreSQL did not cross the timeline "+
			"boundary during replay. Either 2.history was not served when PG probed for it, "+
			"or the TLI-2 segments did not replay. Every fetch-level proof above passed — "+
			"which is exactly why this boot exists.\ngot: %q\nboot log:\n%s",
			rows, lastLines(boot.Logs(ctx), 2500))
	}
	if !t.Failed() {
		t.Logf("proof 4: BOOTED — PG replayed across timelines %d -> %d and served the "+
			"post-failover row", tliBefore, tliAfter)
	}
}

// currentTimeline returns the timeline the primary is WRITING on.
//
// Not pg_control_checkpoint().timeline_id, which is what this test used
// first and which made it flaky: pg_control records the last
// CHECKPOINT's timeline, and a promotion does not force one. Whether it
// reported the new timeline depended on whether a checkpoint happened
// to have run — the first run saw 2, the next saw 1 and failed.
//
// pg_walfile_name(pg_current_wal_lsn()) names the segment being written
// right now, and its first eight hex digits are the current timeline.
// That updates at promotion, with no checkpoint required.
func currentTimeline(t *testing.T, ctx context.Context, topo topology.Topology) uint32 {
	t.Helper()
	name := queryVal(t, ctx, topo, `SELECT pg_walfile_name(pg_current_wal_lsn())`)
	if len(name) < 8 {
		t.Fatalf("pg_walfile_name returned %q, which is not a WAL segment name", name)
	}
	tli, err := strconv.ParseUint(name[:8], 16, 32)
	if err != nil {
		t.Fatalf("parse timeline from %q: %v", name, err)
	}
	return uint32(tli)
}

// leaderAwareDSN folds per-node DSNs into one multi-host DSN that
// routes to whichever node is currently primary.
func leaderAwareDSN(nodes []string) string {
	var hosts []string
	var user string
	for _, d := range nodes {
		at := strings.LastIndex(d, "@")
		scheme := strings.Index(d, "://")
		if at < 0 || scheme < 0 {
			continue
		}
		user = d[scheme+3 : at]
		rest := d[at+1:]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			rest = rest[:slash]
		}
		hosts = append(hosts, rest)
	}
	return fmt.Sprintf("postgres://%s@%s/postgres?target_session_attrs=primary&sslmode=disable",
		user, strings.Join(hosts, ","))
}

// sql runs a statement against whichever node currently leads.
func execSQL(t *testing.T, ctx context.Context, topo topology.Topology, stmt string) {
	t.Helper()
	if _, err := psql(ctx, topo.ConnString(), stmt); err != nil {
		t.Fatalf("sql %q: %v", stmt, err)
	}
}

// queryVal runs a single-value query against the current leader.
func queryVal(t *testing.T, ctx context.Context, topo topology.Topology, q string) string {
	t.Helper()
	out, err := psql(ctx, topo.ConnString(), q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return strings.TrimSpace(out)
}

func psql(ctx context.Context, dsn, stmt string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "psql", dsn, "-tAc", stmt)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

type archivedSeg struct {
	tli  uint32
	name string
}

// archivedSegments lists the committed segment manifests in the repo.
func archivedSegments(t *testing.T, repoDir string) []archivedSeg {
	t.Helper()
	var out []archivedSeg
	base := filepath.Join(repoDir, "wal", "db1")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) != 8 {
			continue
		}
		tli64, perr := strconv.ParseUint(e.Name(), 16, 32)
		if perr != nil {
			continue
		}
		files, ferr := os.ReadDir(filepath.Join(base, e.Name()))
		if ferr != nil {
			continue
		}
		for _, f := range files {
			n := f.Name()
			if !strings.HasSuffix(n, ".json") || strings.Contains(n, ".json.tmp.") {
				continue
			}
			out = append(out, archivedSeg{tli: uint32(tli64), name: strings.TrimSuffix(n, ".json")})
		}
	}
	return out
}

// waitForArchivedTimeline blocks until at least one segment is
// committed on tli, so the assertions run against a caught-up archive
// rather than racing the streamer.
func waitForArchivedTimeline(t *testing.T, ctx context.Context, repoDir string, tli uint32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, s := range archivedSegments(t, repoDir) {
			if s.tli == tli {
				t.Logf("archive reached timeline %d", tli)
				return
			}
		}
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			t.Fatal("context cancelled waiting for the archive to reach the new timeline")
		}
	}
	t.Logf("WARNING: no segment on timeline %d after %s; assertions below will report it", tli, timeout)
}
