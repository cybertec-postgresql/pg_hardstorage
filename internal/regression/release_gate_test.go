// Release-gate roundtrip test.
//
// Single check: the CURRENT binary can take a backup of a real PG,
// verify the backup, and restore it into a fresh datadir.  Failure
// here means the headline backup→verify→restore loop is broken on
// the version about to ship — block the release.
//
// This is intentionally narrow.  It is NOT:
//
//   - A backwards-compat check.  We deliberately don't pin frozen
//     repos from prior versions; the project's compat policy is
//     "current binary works against current repos."
//   - A scenario suite.  Chaos, WAL streaming, sink backends, and
//     PITR live in test/scenarios/L2_* / L3_* / L4_*.  This test
//     is the "is the headline path even functional" gate.
//   - A unit test.  It shells out to the real binary and spins a
//     real Postgres container — runs in `go test -tags release_gate
//     ./internal/regression/...` rather than `go test ./...`.
//
// Skipped (with t.Skip, not t.Fatal) when:
//
//   - The pg_hardstorage binary can't be located.  Local devs who
//     haven't run `make build` should not be punished.
//   - Docker isn't reachable.  Same rationale — environments
//     without Docker should skip cleanly, the same way the
//     existing test-integration target does.
//
// Build tag `release_gate` keeps this out of `go test ./...` by
// default — the test takes ~30s end-to-end (PG container boot
// dominates) and isn't a fast PR-gate signal.  Run via:
//
//   make test-release-gate
//   # or:
//   go test -tags release_gate ./internal/regression/...

//go:build release_gate

package regression

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// resolveBinary picks the pg_hardstorage binary to test against.
// PG_HARDSTORAGE_BIN env override > ../../bin/pg_hardstorage > $PATH.
// Returns "" + skip reason if no binary is available — caller
// t.Skip()s rather than t.Fatal()s so a fresh checkout without a
// build doesn't break the run.
func resolveBinary() (string, string) {
	if v := os.Getenv("PG_HARDSTORAGE_BIN"); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return "", "PG_HARDSTORAGE_BIN: " + err.Error()
		}
		if _, err := os.Stat(abs); err != nil {
			return "", "PG_HARDSTORAGE_BIN does not exist: " + err.Error()
		}
		return abs, ""
	}
	_, thisFile, _, _ := runtime.Caller(0)
	candidate := filepath.Join(filepath.Dir(thisFile), "..", "..", "bin", "pg_hardstorage")
	if abs, err := filepath.Abs(candidate); err == nil {
		if _, err := os.Stat(abs); err == nil {
			return abs, ""
		}
	}
	if path, err := exec.LookPath("pg_hardstorage"); err == nil {
		return path, ""
	}
	return "", "pg_hardstorage binary not found — run `make build` or set PG_HARDSTORAGE_BIN"
}

// runBin shells out to the binary with the given args and a 60s
// budget.  Returns combined stdout+stderr and any exit error.  We
// use CombinedOutput because the structured-error renderer prints
// to stderr; capturing both lets the test's failure message surface
// the actual error code without a separate stderr capture path.
func runBin(t *testing.T, bin string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	return cmd.CombinedOutput()
}

// fatalCmd surfaces a clear failure message including the exit
// code, args, and (truncated) command output.  Avoids the
// "exit status 1" vs "exit status 6 (notfound.backup)" ambiguity
// that bare t.Fatal would produce.
func fatalCmd(t *testing.T, label string, args []string, out []byte, err error) {
	t.Helper()
	ec := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		ec = ee.ExitCode()
	}
	cutoff := 2048
	if len(out) > cutoff {
		out = append(out[:cutoff], []byte("...")...)
	}
	t.Fatalf("%s: pg_hardstorage %s exited %d: %v\noutput:\n%s",
		label, strings.Join(args, " "), ec, err, out)
}

// backupResultBody is the shape we extract from the structured
// `pg_hardstorage backup` output to learn the backup_id.  We pin
// only the field we use; future renderer fields land here without
// breaking the test.
type backupResultBody struct {
	Result struct {
		BackupID string `json:"backup_id"`
	} `json:"result"`
}

// TestReleaseGate_BackupVerifyRestore is the headline release-
// readiness check: spin a fresh PG, take a backup, verify it,
// restore into a fresh dir, assert PG_VERSION exists.  Anything
// in this loop breaking is grounds to block a release.
func TestReleaseGate_BackupVerifyRestore(t *testing.T) {
	bin, skip := resolveBinary()
	if skip != "" {
		t.Skip(skip)
	}

	// Background ctx with a generous outer budget — the PG
	// container takes 5-10s to come up on a warm host, longer
	// on a cold one.  90s gives 3x margin.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pg, err := tcpostgres.Run(ctx, "postgres:17",
		tcpostgres.WithDatabase("releasegate"),
		tcpostgres.WithUsername("releasegate"),
		tcpostgres.WithPassword("releasegate"),
		tc.WithEnv(map[string]string{
			"POSTGRES_INITDB_ARGS": "--data-checksums",
		}),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		// Docker unreachable / image pull failed / similar
		// environmental issue.  Skip rather than fail —
		// matches the existing test-integration semantics
		// (see Makefile's test-integration target for the
		// same Docker-skip pattern).
		t.Skipf("testcontainers PG could not start (Docker unreachable?): %v", err)
	}
	t.Cleanup(func() {
		// 30s tear-down budget; if Terminate hangs the
		// surrounding test framework still cleans up via
		// the Ryuk reaper.
		tearCtx, tearCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer tearCancel()
		_ = pg.Terminate(tearCtx)
	})

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("pg ConnectionString: %v", err)
	}

	repoDir := filepath.Join(t.TempDir(), "repo")
	repoURL := "file://" + repoDir
	deployment := "release-gate"

	// 1. repo init.  Backup refuses an unitialised repo.
	args := []string{"repo", "init", repoURL, "--output", "json"}
	if out, err := runBin(t, bin, args...); err != nil {
		fatalCmd(t, "repo init", args, out, err)
	}

	// 2. backup.  Grab the backup_id from the structured output
	// — the verify+restore steps need it.  We don't use the
	// "latest" alias because that masks a class of bug where
	// the renderer emits the wrong ID and "latest" resolution
	// papers over the discrepancy.
	args = []string{"backup", deployment,
		"--repo", repoURL,
		"--pg-connection", dsn,
		"--output", "json"}
	out, err := runBin(t, bin, args...)
	if err != nil {
		fatalCmd(t, "backup", args, out, err)
	}
	var body backupResultBody
	if perr := json.Unmarshal(out, &body); perr != nil {
		t.Fatalf("backup: parse JSON: %v\noutput:\n%s", perr, out)
	}
	backupID := body.Result.BackupID
	if backupID == "" {
		t.Fatalf("backup: empty backup_id in result\noutput:\n%s", out)
	}
	t.Logf("backup_id: %s", backupID)

	// 3. verify (full chunk SHA-256 round-trip).  Pinning the
	// backup_id rather than passing "latest" — a regression
	// in the latest-resolution path masquerading as a verify
	// pass would slip past "latest".
	args = []string{"verify", deployment, backupID,
		"--repo", repoURL,
		"--output", "json"}
	if out, err := runBin(t, bin, args...); err != nil {
		fatalCmd(t, "verify", args, out, err)
	}

	// 4. verify --existence-only (Stat-only path, separate
	// code path from the full verify).  Catches a regression
	// in just the stat walker without us needing a second
	// fixture.
	args = []string{"verify", deployment, backupID,
		"--repo", repoURL,
		"--existence-only",
		"--output", "json"}
	if out, err := runBin(t, bin, args...); err != nil {
		fatalCmd(t, "verify --existence-only", args, out, err)
	}

	// 5. restore into a fresh target dir.  --verify-restore=off
	// because cluster-start verification needs Docker-in-Docker
	// for the sandbox PG, and we already have a real PG via
	// testcontainers — re-spinning another would double the
	// budget without adding signal.
	target := filepath.Join(t.TempDir(), "restored")
	args = []string{"restore", deployment, backupID,
		"--repo", repoURL,
		"--target", target,
		"--verify-restore", "off",
		"--output", "json"}
	if out, err := runBin(t, bin, args...); err != nil {
		fatalCmd(t, "restore", args, out, err)
	}

	// 6. PG_VERSION sniff: the canonical datadir signature.
	// initdb writes it first; a restore that produced an empty
	// dir or a dir missing PG_VERSION is a chunk-assembly
	// regression.  This is the cheapest invariant that proves
	// "the bytes we wrote actually look like a PG datadir."
	pgVersionPath := filepath.Join(target, "PG_VERSION")
	body2, err := os.ReadFile(pgVersionPath)
	if err != nil {
		t.Fatalf("restored datadir missing PG_VERSION at %s: %v", pgVersionPath, err)
	}
	if v := strings.TrimSpace(string(body2)); v != "17" {
		t.Fatalf("PG_VERSION = %q, want %q (PG version drift in the test fixture or the restore path)",
			v, "17")
	}

	// 7. Spot-check a couple of additional canonical files —
	// the restore lays down many files but pg_control and
	// postgresql.conf are the two most-likely-to-be-broken-
	// silently in a chunk-assembly regression (binary control
	// file vs text config file, different code paths).
	for _, name := range []string{"global/pg_control", "postgresql.conf"} {
		p := filepath.Join(target, name)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("restored datadir missing %s: %v", name, err)
		}
	}
}
