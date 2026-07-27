package cli_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// The dedup-vs-GC race: `repo gc --apply`'s chunk-age floor only
// protects chunks an in-flight backup WROTE (young mtime). Chunks the
// backup DEDUPLICATED against are old by definition — an aged orphan
// whose content reappears gets a dedup hit that never touches the
// object, and the sweep would delete it out from under a backup that
// commits minutes later: a signed, "verified" manifest referencing a
// chunk that no longer exists. The guard: --apply refuses while any
// unexpired backup lease exists.
func TestRepoGC_RefusesWhileBackupLeaseLive(t *testing.T) {
	dir := configDir(t)
	_ = dir

	repoDir := t.TempDir()
	repoURL := "file://" + repoDir
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}

	writeLease := func(deployment string, expires time.Time) {
		t.Helper()
		leaseDir := filepath.Join(repoDir, "leases", deployment)
		if err := os.MkdirAll(leaseDir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"schema":"pg_hardstorage.lease.v1","deployment":%q,"owner":"host/pid-1","acquired_at":%q,"expires_at":%q}`,
			deployment, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
			expires.UTC().Format(time.RFC3339Nano))
		if err := os.WriteFile(filepath.Join(leaseDir, "backup.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Live lease → --apply must refuse with the structured code.
	writeLease("db1", time.Now().Add(10*time.Minute))
	stdout, stderr, exit := runCmd(t, "repo", "gc", repoURL, "--apply", "--output", "json")
	if exit == 0 {
		t.Fatalf("gc --apply succeeded under a live backup lease:\n%s", stdout)
	}
	combined := stdout + stderr
	if !strings.Contains(combined, "repo.gc.live_backup_lease") || !strings.Contains(combined, "db1") {
		t.Errorf("want repo.gc.live_backup_lease naming db1; got:\n%s", combined)
	}

	// Dry-run is read-only and must NOT be blocked by the lease.
	_, _, exit = runCmd(t, "repo", "gc", repoURL, "--output", "json")
	if exit != 0 {
		t.Errorf("dry-run refused under a live lease — it deletes nothing and must not be blocked (exit %d)", exit)
	}

	// Expired lease (crashed holder) → sweep proceeds.
	writeLease("db1", time.Now().Add(-time.Minute))
	stdout, stderr, exit = runCmd(t, "repo", "gc", repoURL, "--apply", "--output", "json")
	if exit != 0 {
		t.Errorf("gc --apply blocked by an EXPIRED lease (exit %d):\n%s%s", exit, stdout, stderr)
	}

	// Unparseable lease → refuse loudly (never "couldn't read the
	// object that says a backup is in flight, deleting anyway").
	if err := os.WriteFile(filepath.Join(repoDir, "leases", "db1", "backup.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, exit = runCmd(t, "repo", "gc", repoURL, "--apply", "--output", "json")
	if exit == 0 {
		t.Errorf("gc --apply proceeded past an unreadable lease:\n%s%s", stdout, stderr)
	}
}
