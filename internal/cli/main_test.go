package cli_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
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
	// One signing keypair in the package's shared keyring, created up
	// front. Read verbs (status, list, repo check, restore, …) verify
	// manifests with it and no longer mint one when it is missing, so
	// tests that do not isolate HOME used to depend on whichever earlier
	// test happened to create it — order-dependent failures under
	// -shuffle. Tests that need a keyring-less (or KEK-less) world set
	// their own HOME and XDG_CONFIG_HOME.
	if p, err := paths.Resolve(paths.DefaultOptions()); err != nil {
		panic(err)
	} else if _, _, err := keystore.LoadOrGenerate(p.Keyring.Value); err != nil {
		panic(err)
	}
	repo.GCFenceSettle = 200 * time.Millisecond
	repo.GCFenceWriterBudget = 100 * time.Millisecond
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
