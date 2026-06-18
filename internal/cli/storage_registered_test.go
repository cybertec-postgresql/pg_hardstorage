package cli_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"

	// Side-effect import path that the production binary
	// transitively pulls in (cli imports backup which imports
	// repo which side-effect-imports each storage backend).
	_ "github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
)

// TestProductionRegistration_StorageFire is the regression
// test for the audit-v26-class wiring bug.  Storage plugins
// register via `storage.Register(scheme, factory)` in
// init().  Without an import chain reaching them, the
// production binary's repo.Open dispatch never knows about
// the scheme — exactly the bug `sftp` had until this commit
// (no init() registration AND no side-effect import).
//
// This test pins down every Tier-1 storage scheme so a
// future refactor that drops a registration is caught at
// test time, not in production.
func TestProductionRegistration_StorageFire(t *testing.T) {
	want := []string{"azblob", "file", "gcs", "s3", "sftp"}
	sort.Strings(want)

	got := storage.Schemes()
	gotSet := map[string]bool{}
	for _, n := range got {
		gotSet[n] = true
	}

	var missing []string
	for _, w := range want {
		if !gotSet[w] {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		t.Errorf("storage schemes not registered in production binary: %s\nregistered: %s",
			strings.Join(missing, ", "),
			strings.Join(got, ", "))
	}
}
