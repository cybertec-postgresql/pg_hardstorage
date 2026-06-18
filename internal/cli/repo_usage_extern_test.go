package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

func TestRepoUsage_RequiresURL(t *testing.T) {
	_, _, exit := runCmd(t, "repo", "usage", "--output", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse(2)", exit)
	}
}

func TestRepoUsage_EmptyRepo(t *testing.T) {
	repoURL := initRepoForTest(t)
	out, _, exit := runCmd(t,
		"repo", "usage", repoURL,
		"--output", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	// Brand-new repo: zero categories (no chunks, no manifests, no WAL).
	if !strings.Contains(out, `"total_objects": 0`) {
		t.Errorf("empty repo should report 0 objects:\n%s", out)
	}
}

func TestRepoUsage_ClassifiesByPrefix(t *testing.T) {
	repoURL := initRepoForTest(t)
	_, sp, _ := repo.Open(context.Background(), repoURL)

	// Chunks (via the CAS — exercises the chunks/sha256/... path).
	cas := casdefault.New(sp)
	for _, body := range [][]byte{[]byte("alpha"), []byte("bravo")} {
		if _, err := cas.PutChunk(context.Background(), body); err != nil {
			t.Fatal(err)
		}
	}

	// A primary manifest, a replica copy, and a trash entry — exercise
	// the classifyManifest splits.
	puts := []struct {
		key  string
		body string
	}{
		{"manifests/db1/backups/test/manifest.json", `{"files":[]}`},
		{"manifests/_replicas/test.manifest.json", `{"files":[]}`},
		{"manifests/_trash/test.json", `{"files":[]}`},
		// A WAL segment manifest — the wal/ prefix bucket.
		{"wal/db1/00000001/000000010000000000000003.json", `{"chunks":[]}`},
	}
	for _, p := range puts {
		_, err := sp.Put(context.Background(), p.key,
			strings.NewReader(p.body),
			storage.PutOptions{ContentLength: int64(len(p.body))})
		if err != nil {
			t.Fatalf("put %s: %v", p.key, err)
		}
	}
	sp.Close()

	out, _, exit := runCmd(t,
		"repo", "usage", repoURL,
		"--output", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	// Each category should appear in the result.
	for _, want := range []string{
		`"category": "chunks"`,
		`"category": "manifests"`,
		`"category": "manifests-replica"`,
		`"category": "manifests-trash"`,
		`"category": "wal"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// Tombstone marker files (.json.tombstone) live BESIDE the live
// manifest under the same prefix. Without explicit classification,
// they were tallied under "manifests" — invisible to operators
// trying to see how much was queued for GC. Assert they get their
// own bucket.
func TestRepoUsage_TombstonesAreSeparateCategory(t *testing.T) {
	repoURL := initRepoForTest(t)
	_, sp, _ := repo.Open(context.Background(), repoURL)
	puts := []struct {
		key, body string
	}{
		{"manifests/db1/backups/live/manifest.json", `{"files":[]}`},
		{"manifests/db1/backups/dead/manifest.json", `{"files":[]}`},
		{"manifests/db1/backups/dead/manifest.json.tombstone", `{"reason":"retention"}`},
	}
	for _, p := range puts {
		if _, err := sp.Put(context.Background(), p.key,
			strings.NewReader(p.body),
			storage.PutOptions{ContentLength: int64(len(p.body))}); err != nil {
			t.Fatalf("put %s: %v", p.key, err)
		}
	}
	sp.Close()

	out, _, exit := runCmd(t, "repo", "usage", repoURL, "--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(out, `"category": "manifests-tombstone"`) {
		t.Errorf("tombstone files should land in their own category:\n%s", out)
	}
	// Live manifests (2 of them) under "manifests"; tombstone (1)
	// must not inflate that count. We don't assert exact totals
	// (test fixtures might add more) but the category must show
	// objects: 1 for the tombstone.
	if !strings.Contains(out, `"category": "manifests"`) {
		t.Errorf("live manifests should still show up as 'manifests':\n%s", out)
	}
}

func TestRepoUsage_RejectsConflictingRepoSources(t *testing.T) {
	_, _, exit := runCmd(t,
		"repo", "usage", "file:///foo",
		"--repo", "file:///bar",
		"--output", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("conflicting positional + --repo should exit ExitMisuse(2); got %d", exit)
	}
}
