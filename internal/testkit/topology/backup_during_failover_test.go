// backup_during_failover_test.go — a base backup whose source is
// demoted mid-flight.
//
// BASE_BACKUP runs over a replication connection to the leader. Patroni
// demotes a leader by restarting PostgreSQL, which kills that
// connection somewhere in the middle of the stream. The question this
// test asks is not whether the backup survives — it should not — but
// whether it FAILS HONESTLY.
//
// The dangerous outcome is a backup that exits 0 having captured only
// part of the data directory. It would appear in `backup list`, satisfy
// retention as though it were a real restore point, and be chosen as
// the base for a recovery that then cannot complete. A backup that is
// known-missing is a problem an operator can act on; a backup that
// claims to exist and does not is one they discover during an incident.
//
// The chaos soak runs backups and switchovers, but as separate rounds —
// nothing there forces the two to overlap. This does.
//
// The test does not assume the outcome. It records what happened and
// asserts only the contract: exit 0 must imply a backup that verifies
// and restores.

//go:build integration && patroni

package topology_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/topology"
)

func TestBackup_InterruptedByAFailover_FailsHonestly(t *testing.T) {
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

	// Enough data that BASE_BACKUP takes long enough to interrupt.
	// Without this the backup finishes before the switchover lands and
	// the test proves nothing.
	//
	// The row count is a function of how fast the host is, not a
	// property of the scenario: 1.5M rows (~768 MB) kept BASE_BACKUP
	// busy for well over the trigger delay on the hardware this was
	// written on, and finished in under 4s on an NVMe box — at which
	// point the test could no longer overlap the two and failed
	// honestly rather than pretending. Raised, and made tunable so a
	// slow machine can dial it back down without editing the test.
	rows := 6000000
	if v := os.Getenv("PGHS_FAILOVER_SEED_ROWS"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n <= 0 {
			t.Fatalf("PGHS_FAILOVER_SEED_ROWS=%q is not a positive integer", v)
		}
		rows = n
	}
	execSQL(t, ctx, topo, fmt.Sprintf(`CREATE TABLE bulk AS
		SELECT g AS id, repeat('x', 512) AS pad FROM generate_series(1, %d) g`, rows))
	execSQL(t, ctx, topo, `CHECKPOINT`)
	t.Logf("seeded %d rows (~%d MB) to slow BASE_BACKUP", rows, rows*512/(1024*1024))

	// Baseline: `init --quick` takes a backup of its own, so "the list
	// is non-empty" says nothing. What matters is whether the
	// INTERRUPTED backup adds an entry it cannot honour.
	baseOut, baseCode := runBin(2*time.Minute, "list", "db1", "--repo", repoURL, "-o", "json")
	if baseCode != 0 {
		t.Fatalf("baseline list failed (%d):\n%s", baseCode, lastLines(baseOut, 1200))
	}
	baseline := map[string]bool{}
	for _, bid := range backupIDs(t, baseOut) {
		baseline[bid] = true
	}
	t.Logf("baseline: %d backup(s) already present before the interrupted run", len(baseline))

	// Repo size before the interrupted run, so the in-flight watch
	// below measures only what THIS backup writes (init --quick already
	// left a backup behind).
	baseRepoBytes := dirBytes(repoDir)

	// Start the backup, then demote its source out from under it.
	type backupOutcome struct {
		out  string
		code int
	}
	done := make(chan backupOutcome, 1)
	go func() {
		out, code := runBin(8*time.Minute, "backup", "db1", "--repo", repoURL, "-o", "json")
		done <- backupOutcome{out, code}
	}()

	backupStart := time.Now()

	// Trigger on evidence, not on a fixed delay. A constant sleep has
	// to be long enough for the slowest host to have started streaming
	// and short enough that the fastest host has not already finished —
	// on fast hardware no such constant exists, and the old 4s lost the
	// overlap entirely. Watch the repo instead and switch over the
	// moment bytes are actually landing, which buys back every second
	// the sleep was spending.
	const inFlightBytes = 8 << 20
	inFlight := false
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); {
		select {
		case res := <-done:
			// The backup finished before we ever saw it streaming. Say
			// so rather than reporting a pass — a scenario that did not
			// happen is not evidence that it is handled.
			t.Fatalf("the backup finished in %s, before the switchover could interrupt it "+
				"(exit %d).\n\nThis test only means something if the two overlap. Raise "+
				"PGHS_FAILOVER_SEED_ROWS (currently %d) so BASE_BACKUP runs longer on this host.",
				time.Since(backupStart).Truncate(time.Millisecond), res.code, rows)
		case <-time.After(100 * time.Millisecond):
		}
		if dirBytes(repoDir) >= baseRepoBytes+inFlightBytes {
			inFlight = true
			break
		}
	}
	if !inFlight {
		t.Fatalf("no backup bytes reached %s within 90s — BASE_BACKUP never started streaming",
			repoDir)
	}

	t.Logf("switching over while the backup is in flight (%s elapsed)",
		time.Since(backupStart).Truncate(time.Millisecond))
	_ = switchover(t, ctx, topo, topo.ConnString())
	t.Logf("switchover returned at %s into the backup",
		time.Since(backupStart).Truncate(time.Millisecond))

	var res backupOutcome
	select {
	case res = <-done:
	case <-time.After(9 * time.Minute):
		t.Fatal("the backup neither completed nor failed within 9 minutes")
	}
	backupRan := time.Since(backupStart)
	t.Logf("=== measured: backup exited %d after %s ===", res.code, backupRan.Truncate(time.Millisecond))

	// The demotion has to land mid-transfer for this to mean anything.
	// If the backup ended within a second of the switchover returning,
	// it may simply have finished first — say so rather than banking a
	// pass on a scenario that did not occur.
	if backupRan < 6*time.Second {
		t.Errorf("the backup ran only %s; the switchover was triggered at 4s, so the "+
			"demotion cannot have landed mid-transfer. Seed more data — this test is not "+
			"exercising the case it describes.", backupRan.Truncate(time.Millisecond))
	}

	// Whatever it decided, `list` is the operator's view of
	// reality and must agree with it.
	listOut, listCode := runBin(2*time.Minute, "list", "db1", "--repo", repoURL, "-o", "json")
	if listCode != 0 {
		t.Fatalf("list failed (%d):\n%s", listCode, lastLines(listOut, 1200))
	}
	ids := backupIDs(t, listOut)
	t.Logf("list reports %d backup(s): %v", len(ids), ids)

	if res.code != 0 {
		// The honest outcome. The only thing left to check is that a
		// failed backup did not leave a usable-looking entry behind:
		// retention counts entries, and a phantom restore point is
		// exactly as misleading as a broken one.
		t.Logf("backup failed cleanly, as it should have")
		var added []string
		for _, bid := range ids {
			if !baseline[bid] {
				added = append(added, bid)
			}
		}
		if len(added) > 0 {
			t.Errorf("the backup exited %d but `list` gained %d new entry/entries: %v\n\n"+
				"A failed backup that leaves a listed entry is a phantom restore point. It "+
				"satisfies retention, it can be selected as a recovery base, and it cannot "+
				"deliver — which the operator discovers during an incident.",
				res.code, len(added), added)
		} else {
			t.Logf("no phantom entry left behind (list unchanged from the baseline)")
		}
		return
	}

	// It claims success. Then it must actually be a backup.
	if len(ids) == 0 {
		t.Fatalf("the backup exited 0 but `list` reports nothing:\n%s",
			lastLines(res.out, 1500))
	}
	// Take the id from the BACKUP COMMAND'S OWN result, not from the
	// list.
	//
	// `list` is newest-first, so indexing from the end picks the OLDEST
	// entry — which here is the backup `init --quick` took before the
	// failover was arranged. An earlier run of this test did exactly
	// that: it verified and restored the wrong backup, never touched
	// the interrupted one, and reported a pass.
	id := backupIDFromResult(t, res.out)
	if id == "" {
		t.Fatalf("the backup exited 0 but its result carries no backup_id; there is nothing "+
			"to verify.\n%s", lastLines(res.out, 1500))
	}
	var listed bool
	for _, got := range ids {
		if got == id {
			listed = true
			break
		}
	}
	if !listed {
		t.Errorf("the backup reported id %q but `list` does not include it (%v).\n\n"+
			"An operator cannot restore what they cannot see.", id, ids)
	}
	t.Logf("examining the INTERRUPTED backup %s", id)

	if out, code := runBin(5*time.Minute, "verify", "db1", id, "--repo", repoURL); code != 0 {
		t.Errorf("the backup exited 0 but `verify %s` fails (exit %d):\n%s\n\n"+
			"A backup that reports success and does not verify is worse than a backup that "+
			"failed: it counts toward retention and will be selected as a recovery base.",
			id, code, lastLines(out, 1500))
	}

	target := filepath.Join(t.TempDir(), "restored")
	if out, code := runBin(5*time.Minute, "restore", "db1", id, "--repo", repoURL,
		"--target", target); code != 0 {
		t.Errorf("the backup exited 0 but `restore %s` fails (exit %d):\n%s",
			id, code, lastLines(out, 1500))
	}
	// A truncated BASE_BACKUP can still produce a manifest that
	// verifies against itself — verification checks what IS listed, not
	// what should have been. These three files are what PG needs to
	// start at all, so their absence is the shape a partial capture
	// takes.
	for _, name := range []string{"PG_VERSION", "global/pg_control", "backup_label"} {
		if _, serr := os.Stat(filepath.Join(target, name)); serr != nil {
			t.Errorf("restored datadir is missing %s: %v\n\n"+
				"The backup reported success and verified, so the manifest is internally "+
				"consistent — it simply does not describe a complete data directory. That is "+
				"what a BASE_BACKUP truncated by the demotion looks like from the outside.",
				name, serr)
		}
	}
	// The assertion that can actually see truncation.
	//
	// verify checks the manifest against itself: a BASE_BACKUP cut
	// short produces a SHORTER manifest that is perfectly consistent
	// with the files it does list, and verifies clean. The only way to
	// notice is to compare against what a complete capture of the same
	// cluster looks like — so take one now, with nothing interfering,
	// and compare file counts.
	if out, code := runBin(8*time.Minute, "backup", "db1", "--repo", repoURL, "-o", "json"); code != 0 {
		t.Fatalf("the control backup failed (%d), so there is nothing to compare against:\n%s",
			code, lastLines(out, 1200))
	}
	listOut2, code2 := runBin(2*time.Minute, "list", "db1", "--repo", repoURL, "-o", "json")
	if code2 != 0 {
		t.Fatalf("list after the control backup failed (%d):\n%s", code2, lastLines(listOut2, 1200))
	}
	counts := backupFileCounts(t, listOut2)
	interrupted := counts[id]
	var control int
	for bid, n := range counts {
		if bid != id && n > control {
			control = n
		}
	}
	t.Logf("file_count: interrupted=%d control=%d", interrupted, control)
	if interrupted == 0 || control == 0 {
		t.Fatalf("could not read file counts (interrupted=%d control=%d); the comparison "+
			"below would be vacuous", interrupted, control)
	}
	// Same cluster moments apart, so a complete capture lands within a
	// few files of the control. A materially smaller manifest means the
	// stream was cut and the shortfall was recorded as success.
	if interrupted < control*9/10 {
		t.Errorf("the interrupted backup lists %d files but a clean backup of the same "+
			"cluster lists %d — roughly %d%% of the data directory is missing, and it "+
			"reported success and verified clean.\n\n"+
			"That is the dangerous shape: a truncated capture whose manifest is internally "+
			"consistent, so verification cannot see it. It counts toward retention and can "+
			"be selected as a recovery base.",
			interrupted, control, 100-(interrupted*100/control))
	}

	if !t.Failed() {
		t.Logf("backup %s claimed success and holds up: verified, restored, and complete "+
			"(%d files vs %d in a clean control)", id, interrupted, control)
	}
}

// backupFileCounts maps backup id -> file_count from `list -o json`.
func backupFileCounts(t *testing.T, out string) map[string]int {
	t.Helper()
	start := strings.Index(out, "{")
	if start < 0 {
		return nil
	}
	var env struct {
		Result struct {
			Backups []struct {
				BackupID  string `json:"backup_id"`
				FileCount int    `json:"file_count"`
			} `json:"backups"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out[start:]), &env); err != nil {
		t.Logf("could not parse list JSON: %v", err)
		return nil
	}
	m := map[string]int{}
	for _, b := range env.Result.Backups {
		m[b.BackupID] = b.FileCount
	}
	return m
}

// backupIDFromResult reads backup_id out of `backup -o json`.
func backupIDFromResult(t *testing.T, out string) string {
	t.Helper()
	start := strings.Index(out, "{")
	if start < 0 {
		return ""
	}
	var env struct {
		Result struct {
			BackupID string `json:"backup_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out[start:]), &env); err != nil {
		t.Logf("could not parse the backup result JSON (%v); raw:\n%s", err, lastLines(out, 800))
		return ""
	}
	return env.Result.BackupID
}

// backupIDs pulls the backup IDs out of `backup list -o json`.
func backupIDs(t *testing.T, out string) []string {
	t.Helper()
	start := strings.Index(out, "{")
	if start < 0 {
		return nil
	}
	var env struct {
		Result struct {
			Backups []struct {
				BackupID  string `json:"backup_id"`
				FileCount int    `json:"file_count"`
			} `json:"backups"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out[start:]), &env); err != nil {
		t.Logf("could not parse backup list JSON (%v); raw:\n%s", err, lastLines(out, 800))
		return nil
	}
	var ids []string
	for _, b := range env.Result.Backups {
		ids = append(ids, b.BackupID)
	}
	return ids
}

// dirBytes totals the bytes under root, ignoring errors: it is a
// progress signal, not an accounting one, and a file vanishing under a
// concurrent writer must not fail the walk.
func dirBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // transient walk errors are not fatal here
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
