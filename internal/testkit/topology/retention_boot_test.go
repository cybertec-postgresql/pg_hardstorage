//go:build integration

package topology_test

// retention_boot_test.go — retention never breaks what it keeps,
// proven at the only level that settles it: PostgreSQL boots the
// surviving backup and replays the pruned archive.
//
// Four earlier retention passes proved which OBJECTS survive the
// janitors — frontier arithmetic, grace windows, chain promotion,
// cross-timeline pruning. All of them assert against the repository.
// None of them ask PostgreSQL. The gap matters because the janitors'
// failure mode is precise: `wal prune` deleting one segment inside the
// kept backup's replay window produces a repo where every object-level
// invariant still holds — the kept manifest verifies, its chunks
// exist — and recovery halts mid-replay. The only assertion that
// closes it is a boot.
//
// So: two real backups with WAL through both, the old one deleted and
// expired, gc + wal prune run WITH real deletions (asserted, not
// assumed — a janitor that deleted nothing proves nothing), then the
// survivor restored --to-latest and BOOTED. The decisive row is
// marker3, written after the kept backup: it exists only in archived
// WAL that the pruned archive must still replay end-to-end.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/pgboot"
)

func TestRetentionThenRecovery_PrunedArchiveStillBoots(t *testing.T) {
	const image = "postgres:17"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if out, err := dockerOut(ctx, "pull", image); err != nil {
		t.Fatalf("pull %s: %v\n%s", image, err, lastLines(out, 300))
	}

	bin := buildProductBinary(t)
	repoDir := mkSharedDir(t, "pghs-retboot-repo-", 0o755)
	spoolDir := mkSharedDir(t, "pghs-retboot-spool-", 0o777)
	targetRoot := mkSharedDir(t, "pghs-retboot-target-", 0o755)
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
	if out, code := runBin(2*time.Minute, "repo", "init", repoURL); code != 0 {
		t.Fatalf("repo init failed (%d):\n%s", code, lastLines(out, 1000))
	}

	src := fmt.Sprintf("pg-hs-retboot-src-%d", time.Now().UnixNano())
	if out, err := dockerOut(ctx, "run", "-d", "--name", src,
		"-p", "127.0.0.1::5432",
		"-e", "POSTGRES_PASSWORD=testkit",
		"-v", spoolDir+":/walspool",
		image,
		"-c", "wal_level=replica",
		"-c", "archive_mode=on",
		"-c", "archive_command=install -m 0644 %p /walspool/%f",
	); err != nil {
		t.Fatalf("start source: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = dockerOut(cctx, "rm", "-f", src)
	})
	psqlSrc := func(sql string) string {
		out, err := dockerOut(ctx, "exec", src, "psql", "-U", "postgres", "-tAc", sql)
		if err != nil {
			t.Fatalf("psql(src) %q: %v\n%s", sql, err, out)
		}
		return strings.TrimSpace(out)
	}
	// Entrypoint-restart-safe readiness — see AwaitVanillaReady.
	pgboot.AwaitVanillaReady(t, ctx, src, 2*time.Minute)
	portOut, err := dockerOut(ctx, "port", src, "5432/tcp")
	if err != nil {
		t.Fatalf("docker port: %v\n%s", err, portOut)
	}
	dsn := fmt.Sprintf("postgres://postgres:testkit@%s/postgres?sslmode=disable",
		strings.TrimSpace(strings.Split(portOut, "\n")[0]))

	// --- Two generations of backup, WAL flowing throughout.
	psqlSrc(`CREATE TABLE ret_proof(id int primary key, note text)`)
	psqlSrc(`INSERT INTO ret_proof VALUES (1, 'before-old-backup')`)
	if out, code := runBin(6*time.Minute, "backup", "db1",
		"--pg-connection", dsn, "--repo", repoURL); code != 0 {
		t.Fatalf("backup #1 failed (%d):\n%s", code, lastLines(out, 1500))
	}
	psqlSrc(`INSERT INTO ret_proof VALUES (2, 'between-backups')`)
	psqlSrc(`SELECT pg_switch_wal()`)
	if out, code := runBin(6*time.Minute, "backup", "db1",
		"--pg-connection", dsn, "--repo", repoURL); code != 0 {
		t.Fatalf("backup #2 failed (%d):\n%s", code, lastLines(out, 1500))
	}
	psqlSrc(`INSERT INTO ret_proof VALUES (3, 'after-kept-backup-only-in-wal')`)
	psqlSrc(`SELECT pg_switch_wal()`)
	psqlSrc(`SELECT pg_switch_wal()`)

	archDeadline := time.Now().Add(2 * time.Minute)
	for {
		if failed := psqlSrc(`SELECT COALESCE(last_failed_wal,'') FROM pg_stat_archiver`); failed != "" {
			t.Fatalf("spool archive_command failing (last_failed_wal=%s):\n%s",
				failed, lastLines(mustLogs(ctx, src), 2000))
		}
		if done := psqlSrc(`SELECT COALESCE(last_archived_wal,'') FROM pg_stat_archiver`); done != "" {
			break
		}
		if time.Now().After(archDeadline) {
			t.Fatalf("nothing archived within budget:\n%s", lastLines(mustLogs(ctx, src), 2000))
		}
		time.Sleep(2 * time.Second)
	}
	entries, derr := os.ReadDir(spoolDir)
	if derr != nil || len(entries) == 0 {
		t.Fatalf("spool empty (err=%v) — nothing to push, nothing to prune, nothing proven", derr)
	}
	for _, e := range entries {
		if out, code := runBin(2*time.Minute, "wal", "push", "db1",
			filepath.Join(spoolDir, e.Name()), "--repo", repoURL); code != 0 {
			t.Fatalf("wal push %s failed (%d):\n%s", e.Name(), code, lastLines(out, 1200))
		}
	}

	// --- Identify the generations and count WAL before the janitors.
	listOut, code := runBin(time.Minute, "list", "db1", "--repo", repoURL, "-o", "text")
	if code != 0 {
		t.Fatalf("list: %d\n%s", code, listOut)
	}
	ids := regexpBackupIDs(listOut)
	if len(ids) != 2 {
		t.Fatalf("want 2 backups, list shows %d:\n%s", len(ids), listOut)
	}
	oldID, keptID := ids[len(ids)-1], ids[0] // list is newest-first
	segsBefore := countSegmentManifests(t, repoDir)

	// --- Retention: the old generation ages out. Grace and age floors
	// zeroed to model "the window passed" (both knobs, deliberately —
	// see the tombstone lifecycle tests for why one is not enough).
	if out, code := runBin(2*time.Minute, "backup", "delete", "db1", oldID,
		"--repo", repoURL, "--reason", "aged out", "--yes"); code != 0 {
		t.Fatalf("delete %s: %d\n%s", oldID, code, lastLines(out, 1000))
	}
	if out, code := runBin(4*time.Minute, "repo", "gc", "--repo", repoURL,
		"--apply", "--tombstone-grace", "0s", "--min-chunk-age", "0s"); code != 0 {
		t.Fatalf("repo gc: %d\n%s", code, lastLines(out, 1500))
	}
	if out, code := runBin(4*time.Minute, "wal", "prune", "db1", "--repo", repoURL,
		"--apply", "--tombstone-grace", "0s"); code != 0 {
		t.Fatalf("wal prune: %d\n%s", code, lastLines(out, 1500))
	}

	// Non-vacuity: the janitors must have DELETED something, or this
	// test is the frontier test again with extra steps.
	segsAfter := countSegmentManifests(t, repoDir)
	if segsAfter >= segsBefore {
		t.Fatalf("wal prune deleted nothing (%d segment manifests before, %d after) — the "+
			"scenario is unstaged: either the frontier never advanced past the old "+
			"generation's WAL or the fixture produced too little WAL to prune", segsBefore, segsAfter)
	}
	t.Logf("janitors acted: %d -> %d segment manifests", segsBefore, segsAfter)

	// --- The kept generation must still recover END TO END.
	if out, err := exec.CommandContext(ctx, "chmod", "-R", "a+rX", repoDir).CombinedOutput(); err != nil {
		t.Fatalf("chmod repo: %v\n%s", err, out)
	}
	target := filepath.Join(targetRoot, "restored")
	if out, code := runBin(6*time.Minute, "restore", "db1", keptID,
		"--repo", repoURL, "--target", target,
		"--to-latest", "--to-action", "promote"); code != 0 {
		t.Fatalf("restore --to-latest of the KEPT backup failed (%d) after the janitors "+
			"ran:\n%s", code, lastLines(out, 1500))
	}
	boot := bootRestoredDatadir(t, ctx, image, target, []string{
		bin + ":" + bin + ":ro",
		repoDir + ":" + repoDir + ":ro",
	}, nil)
	boot.AwaitPromoted(t, ctx, 3*time.Minute)

	got, qerr := boot.Query(ctx, `SELECT note FROM ret_proof ORDER BY id`)
	if qerr != nil {
		t.Fatalf("query after promotion: %v\n%s", qerr, boot.Logs(ctx))
	}
	for _, want := range []string{"before-old-backup", "between-backups", "after-kept-backup-only-in-wal"} {
		if !strings.Contains(got, want) {
			t.Errorf("row %q missing after retention + recovery.\n\n"+
				"The janitors kept every object the kept backup's manifest names — and the "+
				"replay still lost data, which is exactly the failure shape object-level "+
				"retention tests cannot see. got: %q\nboot log:\n%s",
				want, got, lastLines(boot.Logs(ctx), 2000))
		}
	}
	if !t.Failed() {
		t.Logf("retention-then-recovery: pruned archive booted, promoted, and served all "+
			"three markers (kept=%s, expired=%s)", keptID, oldID)
	}
}

// regexpBackupIDs pulls db1.full.* ids out of `list -o text`.
func regexpBackupIDs(out string) []string {
	var ids []string
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, "db1.full.") {
			ids = append(ids, strings.TrimRight(f, ",:"))
		}
	}
	return ids
}

// countSegmentManifests counts committed WAL segment manifests.
func countSegmentManifests(t *testing.T, repoDir string) int {
	t.Helper()
	n := 0
	_ = filepath.Walk(filepath.Join(repoDir, "wal"), func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(p, ".json") && !strings.Contains(p, ".json.tmp.") {
			n++
		}
		return nil
	})
	return n
}
