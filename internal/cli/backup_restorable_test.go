package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
)

// A base backup without --include-wal is only restorable once the WAL
// covering it is archived. With no WAL archive at all — the first-backup
// tutorial's setup — `backup` reported success for backups that restore
// could never finish (recovery waits forever for the next segment). Its
// doctest block even carried a skip note saying so. backup now warns,
// and walArchivedFor is the judgement it rests on: it must say "nothing
// archived" only when that is true, or the warning becomes noise that
// operators learn to ignore.
func TestWalArchivedForJudgesTheRepositoryCorrectly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_ROOT", filepath.Join(dir, "root"))
	repoDir := filepath.Join(dir, "repo")
	repoURL := "file://" + repoDir
	if _, stderr, exit := rotateCLI(t, "repo", "init", repoURL); exit != 0 {
		t.Fatalf("repo init: %s", stderr)
	}
	ctx := context.Background()

	archived, known := walArchivedFor(ctx, repoURL, "db1", 1)
	if !known || archived {
		t.Fatalf("empty repo: archived=%v known=%v, want false/true", archived, known)
	}

	// One archived segment manifest on timeline 1 — what wal stream /
	// wal push leave behind — and archiving is demonstrably working.
	seg := filepath.Join(repoDir, "wal", "db1", "00000001", "000000010000000000000003.json")
	if err := os.MkdirAll(filepath.Dir(seg), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"schema":"` + walsink.Schema + `","end_lsn":"0/4000000"}`
	if err := os.WriteFile(seg, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if archived, known = walArchivedFor(ctx, repoURL, "db1", 1); !known || !archived {
		t.Errorf("with a segment archived: archived=%v known=%v, want true/true — the warning would be a false alarm", archived, known)
	}
	// Another deployment's WAL says nothing about this one.
	if archived, _ = walArchivedFor(ctx, repoURL, "db2", 1); archived {
		t.Error("db1's WAL must not count for db2")
	}
	// A repository that cannot be opened: say nothing rather than guess.
	if _, known = walArchivedFor(ctx, "file://"+filepath.Join(dir, "nope"), "db1", 1); known {
		t.Error("an unopenable repo must report known=false so backup stays silent")
	}
}
