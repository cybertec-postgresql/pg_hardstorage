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
	"os"
	"os/exec"
	"path/filepath"
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
	execSQL(t, ctx, topo, `CREATE TABLE bulk AS
		SELECT g AS id, repeat('x', 512) AS pad FROM generate_series(1, 400000) g`)
	execSQL(t, ctx, topo, `CHECKPOINT`)
	t.Logf("seeded a table large enough to slow BASE_BACKUP")

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

	time.Sleep(4 * time.Second)
	t.Logf("switching over while the backup is in flight")
	_ = switchover(t, ctx, topo, topo.ConnString())

	var res backupOutcome
	select {
	case res = <-done:
	case <-time.After(9 * time.Minute):
		t.Fatal("the backup neither completed nor failed within 9 minutes")
	}
	t.Logf("=== measured: backup exited %d ===", res.code)

	// Whatever it decided, `backup list` is the operator's view of
	// reality and must agree with it.
	listOut, listCode := runBin(2*time.Minute, "backup", "list", "db1", "--repo", repoURL, "-o", "json")
	if listCode != 0 {
		t.Fatalf("backup list failed (%d):\n%s", listCode, lastLines(listOut, 1200))
	}
	ids := backupIDs(t, listOut)
	t.Logf("backup list reports %d backup(s): %v", len(ids), ids)

	if res.code != 0 {
		// The honest outcome. The only thing left to check is that a
		// failed backup did not leave a usable-looking entry behind:
		// retention counts entries, and a phantom restore point is
		// exactly as misleading as a broken one.
		t.Logf("backup failed cleanly, as it should have")
		if len(ids) > 0 {
			t.Errorf("the backup exited %d but `backup list` still reports %d backup(s): %v\n\n"+
				"A failed backup that leaves a listed entry is a phantom restore point. It "+
				"satisfies retention, it can be selected as a recovery base, and it cannot "+
				"deliver — which the operator discovers during an incident.",
				res.code, len(ids), ids)
		}
		return
	}

	// It claims success. Then it must actually be a backup.
	if len(ids) == 0 {
		t.Fatalf("the backup exited 0 but `backup list` reports nothing:\n%s",
			lastLines(res.out, 1500))
	}
	id := ids[len(ids)-1]

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
	t.Logf("backup %s claimed success and holds up: verified and restored", id)
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
				ID string `json:"id"`
			} `json:"backups"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out[start:]), &env); err != nil {
		t.Logf("could not parse backup list JSON (%v); raw:\n%s", err, lastLines(out, 800))
		return nil
	}
	var ids []string
	for _, b := range env.Result.Backups {
		ids = append(ids, b.ID)
	}
	return ids
}
