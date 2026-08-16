package repo_test

// stale_temp_falsepositive_test.go — FindStaleTempManifests must delete ONLY
// genuine commit staging temps (`<realkey>.tmp.<rand>`), NEVER a committed
// manifest. The staging marker is matched in the basename; a committed
// manifest whose PARENT path contains `.json.tmp.` / `.history.tmp.` (valid,
// since validateStorageID permits dots in deployment/backup IDs) must not be
// flagged — `repo gc --apply` deletes what this returns, so a false positive
// is silent data loss. Regression for the mutation_stale_temp_fullkey bug.

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func newStaleTempRepo(t *testing.T) (context.Context, storage.StoragePlugin) {
	t.Helper()
	ctx := context.Background()
	sp := &fs.Plugin{}
	if err := sp.Open(ctx, storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return ctx, sp
}

func putObj(t *testing.T, sp storage.StoragePlugin, ctx context.Context, key string) {
	t.Helper()
	body := []byte("{}")
	if _, err := sp.Put(ctx, key, bytes.NewReader(body), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func TestFindStaleTemp_NeverFlagsLiveManifest(t *testing.T) {
	ctx, sp := newStaleTempRepo(t)

	// LIVE committed manifests whose PARENT path carries the marker.
	live := []string{
		"manifests/db1/backups/evil.json.tmp.x/manifest.json",
		"manifests/db.history.tmp.z/backups/b/manifest.json",
		"wal/dep.json.tmp.q/1/000000010000000000000001.json",
	}
	for _, k := range live {
		putObj(t, sp, ctx, k)
	}
	// GENUINE staging temps (marker in the basename) — must still be reaped.
	temps := []string{
		"manifests/db1/backups/good/manifest.json.tmp.abc123",
		"wal/db1/timelines/2.history.tmp.deadbeef",
	}
	for _, k := range temps {
		putObj(t, sp, ctx, k)
	}

	stale, err := repo.FindStaleTempManifests(ctx, sp, repo.FindOrphansOptions{MinAge: -1})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]struct{}{}
	for _, k := range stale {
		got[k] = struct{}{}
	}
	for _, k := range live {
		if _, bad := got[k]; bad {
			t.Fatalf("LIVE committed manifest %q flagged stale — repo gc --apply would delete it (data loss)", k)
		}
	}
	for _, k := range temps {
		if _, ok := got[k]; !ok {
			t.Fatalf("genuine staging temp %q was NOT flagged — over-correction", k)
		}
	}
}

// FuzzFindStaleTemp_OnlyBasenameMarkedTemps: over arbitrary keys, a key is
// flagged stale IFF its BASENAME carries the staging marker.
func FuzzFindStaleTemp_OnlyBasenameMarkedTemps(f *testing.F) {
	f.Add([]byte{3, 1, 0, 2, 5, 1, 4})
	f.Add([]byte{7, 2, 3, 0, 1, 6, 2, 8})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) < 2 {
			return
		}
		ctx, sp := newStaleTempRepo(t)
		at := func(i int) byte {
			if i < 0 {
				i = -i
			}
			return raw[i%len(raw)]
		}
		// Path fragments, some of which contain the staging marker, used to
		// build parent components AND basenames from fuzz-chosen pieces.
		frag := []string{"db1", "evil.json.tmp.x", "d.history.tmp.z", "backups", "b", "plain"}
		base := []string{"manifest.json", "manifest.json.tmp.r7", "3.history", "2.history.tmp.q9", "seg.json", "x.json.tmp.z"}

		expected := map[string]struct{}{}
		n := int(raw[0]%8) + 1
		seen := map[string]bool{}
		for i := 0; i < n; i++ {
			p1 := frag[int(at(i*3+1))%len(frag)]
			p2 := frag[int(at(i*3+2))%len(frag)]
			b := base[int(at(i*3+3))%len(base)]
			key := "manifests/" + p1 + "/" + p2 + "/" + fmt.Sprintf("u%02d", i) + "/" + b
			if seen[key] {
				continue
			}
			seen[key] = true
			putObj(t, sp, ctx, key)
			// Independent oracle: flagged IFF the BASENAME (b) carries the marker.
			if strings.Contains(b, ".json.tmp.") || strings.Contains(b, ".history.tmp.") {
				expected[key] = struct{}{}
			}
		}

		stale, err := repo.FindStaleTempManifests(ctx, sp, repo.FindOrphansOptions{MinAge: -1})
		if err != nil {
			t.Fatalf("FindStaleTempManifests: %v", err)
		}
		got := map[string]struct{}{}
		for _, k := range stale {
			got[k] = struct{}{}
		}
		for k := range expected {
			if _, ok := got[k]; !ok {
				t.Fatalf("genuine temp %q not flagged", k)
			}
		}
		for k := range got {
			if _, ok := expected[k]; !ok {
				t.Fatalf("key %q flagged stale but its basename carries no staging marker — a "+
					"committed manifest would be deleted by repo gc --apply (DATA LOSS)", k)
			}
		}
	})
}
