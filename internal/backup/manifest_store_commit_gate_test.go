// manifest_store_commit_gate_test.go — regression tests for issue #91.
//
// Issue #91 fired when a malformed manifest (BackupLabel empty
// because pg_basebackup's tarball never carried backup_label)
// committed cleanly, passed basic `verify` (which only re-hashes
// chunks), and only blew up at restore / `verify --full` with
// "manifest.invalid: backup_label is empty".
//
// The fix puts Manifest.Validate() in the Commit() chokepoint so
// EVERY writer — runner, rotate, tests, future code paths — is
// forced through the same gate.  These tests pin that contract:
//
//  1. Commit on a manifest whose Validate fails must return an
//     error and must NOT write the manifest to storage.
//  2. The error path must name the specific invariant that
//     failed (so operators can fix the upstream cause without
//     reading source).
//  3. Each Validate invariant has a separate Commit-refuses case
//     — adding a new invariant to Validate automatically gets
//     regression coverage here as long as the table is kept in
//     sync with the Validate switch.
//
// If anyone ever adds a code path that bypasses Commit (e.g. raw
// Put against the storage plugin), the runtime symptom will be
// the same as #91 — these tests do not catch that case directly
// but the chokepoint placement in Commit() is the structural
// answer.  See manifest_store.go:Commit for the actual gate.
package backup_test

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// commitGateWorld is the smallest fixture that can call
// ManifestStore.Commit end-to-end.
type commitGateWorld struct {
	sp     storage.StoragePlugin
	store  *backup.ManifestStore
	signer *backup.Signer
}

func setupCommitGateWorld(t *testing.T) *commitGateWorld {
	t.Helper()
	root := t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: "file://" + root}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	priv, _, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	return &commitGateWorld{
		sp:     sp,
		store:  backup.NewManifestStore(sp),
		signer: signer,
	}
}

// TestCommit_RefusesInvalidManifest_BackupLabelEmpty is the
// canonical issue #91 regression: a manifest with BackupLabel=""
// commits, then verify --full chokes.  After the fix, Commit
// must refuse with a clear error.
func TestCommit_RefusesInvalidManifest_BackupLabelEmpty(t *testing.T) {
	w := setupCommitGateWorld(t)

	m := validManifest()
	m.BackupLabel = "" // the #91 trigger

	err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{})
	if err == nil {
		t.Fatal("Commit accepted a manifest with empty BackupLabel; issue #91 has regressed")
	}
	if !strings.Contains(err.Error(), "backup_label is empty") {
		t.Errorf("Commit error must name the failed invariant; got %v", err)
	}
	if !strings.Contains(err.Error(), m.BackupID) {
		t.Errorf("Commit error must name the offending backup_id; got %v", err)
	}
}

// TestCommit_RefusesInvalidManifest_TableDriven asserts that
// EACH invariant Validate() checks is also fatal at Commit() —
// not just BackupLabel.  This catches a class of regressions
// where someone adds a new invariant to Validate but forgets to
// keep Commit honest about it.
//
// Keep this table in sync with Manifest.Validate's switch cases.
// New invariant → new row here.
func TestCommit_RefusesInvalidManifest_TableDriven(t *testing.T) {
	cases := []struct {
		name            string
		mutate          func(*backup.Manifest)
		wantErrSub      string // substring expected in Commit's error
		wantValidateSub string // substring expected in Validate's error (defaults to wantErrSub when empty)
	}{
		{
			name:       "schema mismatch",
			mutate:     func(m *backup.Manifest) { m.Schema = "pg_hardstorage.manifest.v999" },
			wantErrSub: "schema",
		},
		{
			// Caught by Commit's own pre-check before Validate;
			// the test asserts Commit refuses, regardless of
			// which check fires first.
			name:            "backup_id empty",
			mutate:          func(m *backup.Manifest) { m.BackupID = "" },
			wantErrSub:      "BackupID",
			wantValidateSub: "backup_id is empty",
		},
		{
			// Same: caught by Commit's pre-check.
			name:            "deployment empty",
			mutate:          func(m *backup.Manifest) { m.Deployment = "" },
			wantErrSub:      "Deployment",
			wantValidateSub: "deployment is empty",
		},
		{
			name:       "pg_version zero",
			mutate:     func(m *backup.Manifest) { m.PGVersion = 0 },
			wantErrSub: "pg_version",
		},
		{
			name:       "system_identifier empty",
			mutate:     func(m *backup.Manifest) { m.SystemIdentifier = "" },
			wantErrSub: "system_identifier is empty",
		},
		{
			name:       "start_lsn empty",
			mutate:     func(m *backup.Manifest) { m.StartLSN = "" },
			wantErrSub: "LSN",
		},
		{
			name:       "stop_lsn empty",
			mutate:     func(m *backup.Manifest) { m.StopLSN = "" },
			wantErrSub: "LSN",
		},
		{
			name:       "backup_label empty (#91)",
			mutate:     func(m *backup.Manifest) { m.BackupLabel = "" },
			wantErrSub: "backup_label is empty",
		},
		{
			name:       "no tablespaces",
			mutate:     func(m *backup.Manifest) { m.Tablespaces = nil },
			wantErrSub: "no tablespaces",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := setupCommitGateWorld(t)
			m := validManifest()
			tc.mutate(m)

			err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{})
			if err == nil {
				t.Fatalf("Commit accepted a manifest that fails Validate (%s); chokepoint regressed", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Errorf("Commit error = %v; want substring %q", err, tc.wantErrSub)
			}

			// Also confirm the same invariant fires directly on
			// Validate — if these ever drift, the gate isn't
			// actually wired to Validate any more.
			wantValidateSub := tc.wantValidateSub
			if wantValidateSub == "" {
				wantValidateSub = tc.wantErrSub
			}
			if vErr := m.Validate(); vErr == nil {
				t.Errorf("Validate accepted the same mutation; table is out of sync with Validate")
			} else if !strings.Contains(vErr.Error(), wantValidateSub) {
				t.Errorf("Validate error = %v; want substring %q", vErr, wantValidateSub)
			}
		})
	}
}

// TestCommit_RefusesInvalidManifest_NoWrite asserts that a
// refused Commit must not have written the manifest to storage.
// A half-written manifest (object present, sidecar missing,
// retention applied) is the worst possible failure mode because
// it would survive a retry without surfacing the original error.
func TestCommit_RefusesInvalidManifest_NoWrite(t *testing.T) {
	w := setupCommitGateWorld(t)

	m := validManifest()
	m.BackupLabel = ""

	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err == nil {
		t.Fatal("Commit must reject")
	}

	// Manifest must not be readable; ManifestStore.Read returns
	// the storage plugin's NotFound for never-written manifests.
	_, err := w.store.Read(context.Background(), m.Deployment, m.BackupID, nil)
	if err == nil {
		t.Fatal("Commit refused but the manifest is readable; refused Commit must leave no on-disk trace")
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestCommit_AcceptsValidManifest is the happy-path counter-
// weight: with the gate in place, valid manifests still go
// through.  Without this case, the table-driven negative tests
// could trivially pass by Commit always returning an error.
func TestCommit_AcceptsValidManifest(t *testing.T) {
	w := setupCommitGateWorld(t)
	m := validManifest()
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("Commit refused a valid manifest: %v", err)
	}
}
