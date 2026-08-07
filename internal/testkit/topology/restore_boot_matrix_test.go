//go:build integration

package topology_test

// restore_boot_matrix_test.go — the full loop, per PostgreSQL major:
// backup → archive WAL → restore --to-latest → BOOT → query the row
// that only exists in archived WAL.
//
// Everything before this proved pieces: segments fetchable, manifests
// verified, files restored. None of it proves PostgreSQL itself will
// accept what we hand it — replay our archived WAL through
// restore_command to the end, promote, and serve the data. That
// acceptance is version-specific (WAL format, control file,
// backup_label semantics all move between majors), so it is proven per
// major, in a version-exact container.
//
// The decisive assertion is marker2: a row inserted AFTER the backup,
// reachable only by fetching archived segments through the product's
// own restore_command. A boot that serves marker1 but not marker2 is a
// backup restore; serving marker2 is archive REPLAY — the thing
// --to-latest exists for.
//
// Majors 16, 17 and 18 are required — their images are established,
// and a pull failure is an environment fault worth failing on. 19 is
// attempted and skips LOUDLY if no image exists yet (pre-release
// availability), with PGHS_PG19_IMAGE as the override for beta tags.

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

func TestRestoreBootMatrix_ArchiveReplayAcrossMajors(t *testing.T) {
	for _, major := range []string{"16", "17", "18", "19"} {
		major := major
		t.Run("pg"+major, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()

			// Established majors have exactly one candidate tag and an
			// unpullable image is an environment fault. A PRE-RELEASE
			// major is probed across the tags its images actually ship
			// under (final, then betas, newest first) so the lane starts
			// covering it the day a beta image exists — with
			// PGHS_PG<major>_IMAGE as the explicit override for anything
			// else. Only when every candidate fails does the lane skip,
			// loudly, saying the version was NOT verified.
			candidates := []string{"postgres:" + major}
			preRelease := major == "19"
			if preRelease {
				if v := os.Getenv("PGHS_PG" + major + "_IMAGE"); v != "" {
					candidates = []string{v}
				} else {
					candidates = append(candidates,
						"postgres:"+major+"rc1",
						"postgres:"+major+"beta3",
						"postgres:"+major+"beta2",
						"postgres:"+major+"beta1")
				}
			}
			image, pullLog := "", ""
			for _, cand := range candidates {
				out, err := dockerOut(ctx, "pull", cand)
				if err == nil {
					image = cand
					break
				}
				pullLog += cand + ": " + lastLines(out, 120) + "\n"
			}
			if image == "" {
				if preRelease {
					t.Skipf("SKIP (environment, loud): no pullable PostgreSQL %s image among "+
						"%v — pre-release availability. This skip means PG %s was NOT "+
						"verified.\n%s", major, candidates, major, pullLog)
				}
				t.Fatalf("no pullable image among %v — majors 16-18 are established; this is "+
					"an environment fault, not a version gap:\n%s", candidates, pullLog)
			}
			if image != candidates[0] {
				t.Logf("pg%s: using pre-release image %s", major, image)
			}
			runBootMatrixFor(t, ctx, major, image)
		})
	}
}

func runBootMatrixFor(t *testing.T, ctx context.Context, major, image string) {
	bin := buildProductBinary(t)

	// Directory layout is dictated by uid arithmetic, and the first
	// version of this test got it wrong twice, so the constraints are
	// spelled out:
	//
	//   - Go's t.TempDir() creates 0700 directories owned by the test
	//     user. The container's postgres runs as uid 999 and cannot
	//     even TRAVERSE them — which surfaced first as "create the
	//     repository first" (an EACCES Stat reading as not-found) and
	//     then as HSREPO permission denied.
	//   - Mixed-uid writes into ONE repo (999 pushing WAL, host user
	//     writing manifests) would need every internal file world-
	//     writable. Instead the container archives into a 0777 SPOOL
	//     with plain cp, and the HOST pushes the spooled segments
	//     through the real `wal push` — same parser, same commit path,
	//     same per-major segment headers; only the invoking uid
	//     differs.
	//   - The BOOT container's restore_command must READ the repo as
	//     uid 999, so the repo is chmod'd a+rX (read-only mount) before
	//     boot.
	repoDir := mkSharedDir(t, "pghs-matrix-repo-", 0o755)
	spoolDir := mkSharedDir(t, "pghs-matrix-spool-", 0o777)
	targetRoot := mkSharedDir(t, "pghs-matrix-target-", 0o755)
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

	// --- Source server, with the product binary and the repo mounted
	// at their HOST paths so one archive_command works both inside the
	// container (wal push) and on the host (restore_command later).
	src := fmt.Sprintf("pg-hs-matrix-src-%s-%d", major, time.Now().UnixNano())
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
	// Readiness: the entrypoint restarts once after initdb, and
	// pg_isready alone can catch the TEMPORARY init-phase server —
	// the pg16 leg of the pre-release soak died with "the database
	// system is shutting down" on exactly that race. The pgboot
	// helper gates on the entrypoint's init-complete marker first.
	pgboot.AwaitVanillaReady(t, ctx, src, 2*time.Minute)

	// The host CLI reaches the source via the published port — the
	// same pattern every other topology test uses. (The first version
	// inspected the bridge IP, which is empty on hosts whose default
	// network is not the classic bridge.)
	portOut, err := dockerOut(ctx, "port", src, "5432/tcp")
	if err != nil {
		t.Fatalf("docker port %s: %v\n%s", src, err, portOut)
	}
	hostPort := strings.TrimSpace(strings.Split(portOut, "\n")[0])
	dsn := fmt.Sprintf("postgres://postgres:testkit@%s/postgres?sslmode=disable", hostPort)

	// marker1 exists in the BACKUP; marker2 exists only in ARCHIVED WAL.
	psqlSrc(`CREATE TABLE boot_proof(id int primary key, note text)`)
	psqlSrc(`INSERT INTO boot_proof VALUES (1, 'in-the-backup')`)
	if out, code := runBin(6*time.Minute, "backup", "db1",
		"--pg-connection", dsn, "--repo", repoURL); code != 0 {
		t.Fatalf("backup failed (%d):\n%s", code, lastLines(out, 1500))
	}
	psqlSrc(`INSERT INTO boot_proof VALUES (2, 'only-in-archived-wal')`)
	psqlSrc(`SELECT pg_switch_wal()`)
	// A second switch guarantees the segment holding marker2 is closed
	// and handed to archive_command, not merely eligible.
	psqlSrc(`SELECT pg_switch_wal()`)

	// Wait for the archiver to drain: last_archived_wal advances.
	archDeadline := time.Now().Add(2 * time.Minute)
	for {
		failed := psqlSrc(`SELECT COALESCE(last_failed_wal,'') FROM pg_stat_archiver`)
		done := psqlSrc(`SELECT COALESCE(last_archived_wal,'') FROM pg_stat_archiver`)
		if failed != "" {
			t.Fatalf("the install-to-spool archive_command is failing (last_failed_wal=%s) — a "+
				"fixture mount fault:\n%s", failed, lastLines(mustLogs(ctx, src), 2500))
		}
		if done != "" {
			break
		}
		if time.Now().After(archDeadline) {
			t.Fatalf("nothing archived within budget:\n%s", lastLines(mustLogs(ctx, src), 2500))
		}
		time.Sleep(2 * time.Second)
	}

	// --- Host pushes every spooled file through the REAL wal push.
	// Per-major segment headers (xlp magic changes across majors) flow
	// through the product's parser here; only the invoking uid differs
	// from an in-container archive_command.
	entries, derr := os.ReadDir(spoolDir)
	if derr != nil {
		t.Fatal(derr)
	}
	if len(entries) == 0 {
		t.Fatal("spool is empty after the archiver drained — the fixture is broken and " +
			"nothing below would test replay")
	}
	pushed := 0
	for _, e := range entries {
		if out, code := runBin(2*time.Minute, "wal", "push", "db1",
			filepath.Join(spoolDir, e.Name()), "--repo", repoURL); code != 0 {
			t.Fatalf("wal push %s failed (%d):\n%s", e.Name(), code, lastLines(out, 1200))
		}
		pushed++
	}
	t.Logf("pg%s: pushed %d spooled WAL file(s) through the product parser", major, pushed)

	// The boot container reads the repo as uid 999.
	if out, err := exec.CommandContext(ctx, "chmod", "-R", "a+rX", repoDir).CombinedOutput(); err != nil {
		t.Fatalf("chmod repo: %v\n%s", err, out)
	}

	// --- Restore with end-of-archive recovery, then BOOT it.
	target := filepath.Join(targetRoot, "restored")
	if out, code := runBin(6*time.Minute, "restore", "db1", "latest",
		"--repo", repoURL, "--target", target,
		"--to-latest", "--to-action", "promote"); code != 0 {
		t.Fatalf("restore --to-latest failed (%d):\n%s", code, lastLines(out, 1500))
	}

	boot := bootRestoredDatadir(t, ctx, image, target, []string{
		bin + ":" + bin + ":ro",
		repoDir + ":" + repoDir + ":ro",
	}, nil)
	boot.AwaitPromoted(t, ctx, 3*time.Minute)

	got, qerr := boot.Query(ctx, `SELECT note FROM boot_proof ORDER BY id`)
	if qerr != nil {
		t.Fatalf("query after promotion: %v\n%s", qerr, boot.Logs(ctx))
	}
	if !strings.Contains(got, "in-the-backup") {
		t.Errorf("pg%s: marker1 missing — the RESTORE itself is broken:\n%q", major, got)
	}
	if !strings.Contains(got, "only-in-archived-wal") {
		t.Errorf("pg%s: marker2 missing.\n\nThe backup restored and the server promoted, but "+
			"the row that lives only in ARCHIVED WAL never arrived — PostgreSQL %s did not "+
			"replay our archive through restore_command to the end. Every upstream proof "+
			"(segments fetchable, manifests verified) passed while the thing operators "+
			"actually need — the data — did not come back.\ngot: %q\nboot log:\n%s",
			major, major, got, lastLines(boot.Logs(ctx), 2000))
	}
	t.Logf("pg%s: booted, promoted, and served both markers (archive replay proven)", major)
}

func mustLogs(ctx context.Context, name string) string {
	out, _ := dockerOut(ctx, "logs", "--tail", "120", name)
	return out
}
