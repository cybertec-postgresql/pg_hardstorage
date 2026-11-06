package repo_test

import (
	"context"
	"errors"
	"fmt"
	stdio "io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func tempFileURL(t *testing.T) string {
	t.Helper()
	return "file://" + t.TempDir()
}

func TestInit_FreshURL(t *testing.T) {
	ctx := context.Background()
	url := tempFileURL(t)
	res, err := repo.Init(ctx, repo.InitOptions{URL: url})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if res.Schema != repo.SchemaRepo {
		t.Errorf("schema = %q", res.Schema)
	}
	if len(res.ID) != 32 {
		t.Errorf("id should be 32 hex chars; got %q (len %d)", res.ID, len(res.ID))
	}
	if res.Metadata.CreatedAt == "" {
		t.Error("created_at should be set")
	}
}

func TestInit_AlreadyExists(t *testing.T) {
	ctx := context.Background()
	url := tempFileURL(t)
	if _, err := repo.Init(ctx, repo.InitOptions{URL: url}); err != nil {
		t.Fatal(err)
	}
	_, err := repo.Init(ctx, repo.InitOptions{URL: url})
	if err == nil {
		t.Fatal("second init should fail")
	}
	if err != repo.ErrAlreadyExists {
		t.Errorf("expected ErrAlreadyExists; got %v", err)
	}
}

func TestInit_RaceProducesExactlyOneRepo(t *testing.T) {
	ctx := context.Background()
	url := tempFileURL(t)
	var wins atomic.Int32
	var wg sync.WaitGroup
	const N = 8
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			if _, err := repo.Init(ctx, repo.InitOptions{URL: url}); err == nil {
				wins.Add(1)
			} else if err != repo.ErrAlreadyExists {
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := wins.Load(); got != 1 {
		t.Errorf("expected exactly one Init winner; got %d", got)
	}
}

func TestOpen_EmptyURL_NotARepo(t *testing.T) {
	ctx := context.Background()
	url := tempFileURL(t) // dir exists but no HSREPO
	_, _, err := repo.Open(ctx, url)
	if err != repo.ErrNotARepo {
		t.Errorf("expected ErrNotARepo; got %v", err)
	}
}

func TestInitOpen_RoundTrip(t *testing.T) {
	ctx := context.Background()
	url := tempFileURL(t)
	got, err := repo.Init(ctx, repo.InitOptions{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	meta, sp, err := repo.Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sp.Close()
	if meta.ID != got.ID {
		t.Errorf("id round-trip: got %q want %q", meta.ID, got.ID)
	}
	// Stat the HSREPO via the same plugin to confirm it's readable.
	rc, err := sp.Get(ctx, repo.HSREPOFilename)
	if err != nil {
		t.Fatalf("get HSREPO: %v", err)
	}
	defer rc.Close()
	body, _ := stdio.ReadAll(rc)
	if !strings.Contains(string(body), got.ID) {
		t.Errorf("HSREPO body should contain id; got %s", body)
	}
}

func TestInit_UnknownScheme(t *testing.T) {
	_, err := repo.Init(context.Background(), repo.InitOptions{URL: "weirdo://nope"})
	if err == nil {
		t.Fatal("unknown scheme should fail")
	}
}

// A damaged HSREPO is the repository telling us it is broken, not a bug
// in this tool: Open must say so with a structured verify.* error (exit
// 9) that survives the %w wrapping its many callers apply, instead of a
// bare JSON parse error that the CLI files under "internal".
func TestOpen_DamagedMarkerIsStructured(t *testing.T) {
	ctx := context.Background()
	for name, body := range map[string]string{
		"not json":       "not json",
		"truncated":      `{"schema":"pg_hardstorage.repo.v1","id":"ab`,
		"empty":          "",
		"json, no shape": `{}`,
	} {
		url := tempFileURL(t)
		path := filepath.Join(strings.TrimPrefix(url, "file://"), repo.HSREPOFilename)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, sp, err := repo.Open(ctx, url)
		if err == nil {
			_ = sp.Close()
			t.Errorf("%s: Open accepted a damaged marker", name)
			continue
		}
		if !errors.Is(err, repo.ErrCorruptMarker) {
			t.Errorf("%s: err = %v, want ErrCorruptMarker in the chain", name, err)
		}
		oe, ok := output.AsOutputError(fmt.Errorf("open repo: %w", err))
		if !ok || oe.Code != "verify.repo_marker_corrupt" {
			t.Errorf("%s: code = %v (structured=%v), want verify.repo_marker_corrupt", name, oe, ok)
		}
		if ok && oe.Suggestion == nil {
			t.Errorf("%s: no suggestion on the most actionable repository failure there is", name)
		}
		if errors.Is(err, repo.ErrNotARepo) {
			t.Errorf("%s: a damaged marker must not read as \"no repository here\"", name)
		}
	}
}
