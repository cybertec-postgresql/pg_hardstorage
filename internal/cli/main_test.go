package cli_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestMain isolates every CLI test from the developer's / CI runner's
// ambient ~/.config/pg_hardstorage. Now that deployment-scoped commands
// resolve --repo / --pg-connection from the deployment catalogue (#12), a
// real "db1" (or any) deployment present on the machine would otherwise
// satisfy those flags in tests that mean to assert the flag is required,
// making them pass or fail depending on the host. A clean, empty HOME
// makes the whole package deterministic; individual tests still inject
// their own config via t.Setenv as needed.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "hs-cli-test-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	os.Unsetenv("PG_HARDSTORAGE_CONFIG")
	os.Unsetenv("PG_HARDSTORAGE_CONFIG_DIR")
	os.Unsetenv("PG_HARDSTORAGE_ROOT")
	// `repo gc --apply` waits GCFenceSettle (60s) before its deciding
	// snapshot so writers that read "no gc running" can finish their
	// commit; nothing in this package commits concurrently with a gc
	// except the tests that set these explicitly, so compress it.
	repo.GCFenceSettle = 200 * time.Millisecond
	repo.GCFenceWriterBudget = 100 * time.Millisecond
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
