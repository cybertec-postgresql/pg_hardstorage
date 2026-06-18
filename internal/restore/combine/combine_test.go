package combine_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/combine"
)

// execLookerForTest is a tiny indirection used by version-detection
// tests so they avoid importing os/exec at every test site.
func execLookerForTest(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// stripBeforePostgres trims everything up through "PostgreSQL) " so
// Sscanf can read the major directly.
func stripBeforePostgres(s string) string {
	const marker = "PostgreSQL) "
	i := strings.Index(s, marker)
	if i < 0 {
		return s
	}
	return s[i+len(marker):]
}

// newRepo wires a temp file:// repository, a signer, and a verifier
// for committing manifests in tests.
func newRepo(t *testing.T) (storage.StoragePlugin, *backup.Signer, *backup.Verifier) {
	t.Helper()
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatalf("fs open: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := backup.LoadSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := backup.LoadVerifier(pub)
	if err != nil {
		t.Fatal(err)
	}
	return sp, signer, verifier
}

// commitManifest signs m and writes it DIRECTLY to its primary key so
// it's readable by Build. It bypasses ManifestStore.Commit on purpose:
// several of these tests deliberately plant malformed / corrupt chains
// (missing parent, multi-node cycles) — the exact on-disk states
// Build must detect — which Commit's chain parent-liveness guard now
// (correctly) refuses to create. Writing the signed body straight to
// storage simulates that on-disk corruption.
func commitManifest(t *testing.T, sp storage.StoragePlugin, signer *backup.Signer, m *backup.Manifest) {
	t.Helper()
	if m.Attestation == nil {
		if err := m.Sign(signer); err != nil {
			t.Fatalf("sign %s: %v", m.BackupID, err)
		}
	}
	body, err := m.MarshalToBytes()
	if err != nil {
		t.Fatalf("marshal %s: %v", m.BackupID, err)
	}
	key := backup.PrimaryPath(m.Deployment, m.BackupID)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(body),
		storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("put manifest %s: %v", m.BackupID, err)
	}
}

// mkFull builds a minimal full-backup manifest.
func mkFull(id string, stoppedAt time.Time) *backup.Manifest {
	return &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       "db1",
		Type:             backup.BackupTypeFull,
		PGVersion:        170,
		SystemIdentifier: "7388123456789012345",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		StartedAt:        stoppedAt.Add(-time.Minute),
		StoppedAt:        stoppedAt,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		PGBackupManifest: []byte(`{"PostgreSQL-Backup-Manifest-Version":1}`),
	}
}

// mkInc builds an incremental manifest pointing at parentID.
func mkInc(id, parentID string, stoppedAt time.Time) *backup.Manifest {
	m := mkFull(id, stoppedAt)
	m.Type = backup.BackupTypeIncremental
	m.ParentBackupID = parentID
	return m
}

// TestBuild_FullOnlyChain: a leaf that's itself a full backup yields
// a chain of length 1. IsIncremental is false; the caller should
// route to the regular full-restore path.
func TestBuild_FullOnlyChain(t *testing.T) {
	sp, signer, verifier := newRepo(t)
	full := mkFull("db1.full.20260428T1200Z", time.Now().UTC())
	commitManifest(t, sp, signer, full)

	chain, err := combine.Build(context.Background(), sp, "db1", full.BackupID, verifier)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("len(chain)=%d, want 1", len(chain))
	}
	if chain.IsIncremental() {
		t.Errorf("single-link chain should not report incremental")
	}
	if chain[0].BackupID != full.BackupID {
		t.Errorf("chain[0] = %q, want %q", chain[0].BackupID, full.BackupID)
	}
}

// TestBuild_IncrementalChainOrder: a 3-link chain (full → inc1 → inc2)
// returns [full, inc1, inc2] in restore order.
func TestBuild_IncrementalChainOrder(t *testing.T) {
	sp, signer, verifier := newRepo(t)
	t0 := time.Now().UTC().Add(-3 * time.Hour)
	full := mkFull("db1.full.20260428T0900Z", t0)
	inc1 := mkInc("db1.incremental_lsn.20260428T1000Z", full.BackupID, t0.Add(time.Hour))
	inc2 := mkInc("db1.incremental_lsn.20260428T1100Z", inc1.BackupID, t0.Add(2*time.Hour))
	commitManifest(t, sp, signer, full)
	commitManifest(t, sp, signer, inc1)
	commitManifest(t, sp, signer, inc2)

	chain, err := combine.Build(context.Background(), sp, "db1", inc2.BackupID, verifier)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !chain.IsIncremental() {
		t.Errorf("3-link chain should report incremental")
	}
	wantIDs := []string{full.BackupID, inc1.BackupID, inc2.BackupID}
	for i, m := range chain {
		if m.BackupID != wantIDs[i] {
			t.Errorf("chain[%d] = %q, want %q", i, m.BackupID, wantIDs[i])
		}
	}
}

// TestBuild_MissingLink: a chain whose parent is not committed
// surfaces a notfound-shaped error.
func TestBuild_MissingLink(t *testing.T) {
	sp, signer, verifier := newRepo(t)
	t0 := time.Now().UTC()
	// Commit only the leaf incremental; the parent ID is bogus.
	inc := mkInc("db1.incremental_lsn.20260428T1100Z", "db1.full.does-not-exist", t0)
	commitManifest(t, sp, signer, inc)

	_, err := combine.Build(context.Background(), sp, "db1", inc.BackupID, verifier)
	if err == nil {
		t.Fatal("expected error for missing parent link")
	}
	// The error should mention the missing parent ID so an operator
	// can search for it.
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the missing parent: %v", err)
	}
}

// TestBuild_TombstonedAncestor: an ancestor that was tombstoned
// surfaces the structured chain.broken_tombstoned code with a
// remediation suggestion.
//
// SoftDelete itself now refuses to tombstone a manifest with live
// incremental descendants (chain protection). To still cover the
// read-side diagnostic (defence-in-depth: a hand-edited tombstone,
// or a future rotate-bug that bypasses the protection), we construct
// the broken state by direct storage write — bypassing SoftDelete.
func TestBuild_TombstonedAncestor(t *testing.T) {
	sp, signer, verifier := newRepo(t)
	t0 := time.Now().UTC().Add(-2 * time.Hour)
	full := mkFull("db1.full.20260428T0900Z", t0)
	inc := mkInc("db1.incremental_lsn.20260428T1000Z", full.BackupID, t0.Add(time.Hour))
	commitManifest(t, sp, signer, full)
	commitManifest(t, sp, signer, inc)

	// Direct write of the tombstone marker. SoftDelete would refuse
	// here (chain has live descendants), which is the correct
	// production behaviour; we bypass it to exercise the defence-
	// in-depth diagnostic path.
	tombstoneKey := backup.TombstonePath("db1", full.BackupID)
	if _, err := sp.Put(context.Background(), tombstoneKey,
		strings.NewReader(`{"schema":"pg_hardstorage.tombstone.v1"}`),
		storage.PutOptions{}); err != nil {
		t.Fatalf("direct tombstone write: %v", err)
	}

	_, err := combine.Build(context.Background(), sp, "db1", inc.BackupID, verifier)
	if err == nil {
		t.Fatal("expected chain.broken_tombstoned error")
	}
	var oerr *output.Error
	if !errors.As(err, &oerr) || oerr.Code != "chain.broken_tombstoned" {
		t.Errorf("expected chain.broken_tombstoned, got %v", err)
	}
}

// TestBuild_RejectsLeafID_Empty: empty input is a misuse.
func TestBuild_RejectsLeafID_Empty(t *testing.T) {
	sp, _, verifier := newRepo(t)
	_, err := combine.Build(context.Background(), sp, "db1", "", verifier)
	if err == nil {
		t.Error("expected error for empty leafID")
	}
}

// TestBuild_RejectsCycle: a chain whose parent points back to the
// leaf surfaces chain.cycle. We simulate a cycle via a manually-
// constructed manifest whose ParentBackupID equals its own id.
func TestBuild_RejectsCycle(t *testing.T) {
	sp, signer, verifier := newRepo(t)
	t0 := time.Now().UTC()
	self := mkFull("db1.full.cycle", t0)
	self.Type = backup.BackupTypeIncremental
	self.ParentBackupID = self.BackupID // self-loop
	commitManifest(t, sp, signer, self)

	_, err := combine.Build(context.Background(), sp, "db1", self.BackupID, verifier)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	var oerr *output.Error
	if !errors.As(err, &oerr) || oerr.Code != "chain.cycle" {
		t.Errorf("expected chain.cycle, got %v", err)
	}
}

// TestBuild_NoFullAnchor: a chain whose root link is itself an
// incremental with no parent (someone hand-edited a manifest)
// surfaces chain.no_full_anchor. Defensive — should be unreachable
// in normal operation since the runner only sets Type=incremental
// when ParentBackupID is also set.
func TestBuild_NoFullAnchor(t *testing.T) {
	sp, signer, verifier := newRepo(t)
	t0 := time.Now().UTC()
	rogue := mkFull("db1.incremental_lsn.rogue", t0)
	rogue.Type = backup.BackupTypeIncremental
	rogue.ParentBackupID = "" // no parent + incremental type = malformed
	commitManifest(t, sp, signer, rogue)

	_, err := combine.Build(context.Background(), sp, "db1", rogue.BackupID, verifier)
	if err == nil {
		t.Fatal("expected chain.no_full_anchor error")
	}
	var oerr *output.Error
	if !errors.As(err, &oerr) || oerr.Code != "chain.no_full_anchor" {
		t.Errorf("expected chain.no_full_anchor, got %v", err)
	}
}

// TestRun_RejectsBadInputs: at least 2 dirs + non-empty OutputDir
// are required; the binary itself must exist on PATH (or the
// pre-flight surfaces a structured error).
func TestRun_RejectsBadInputs(t *testing.T) {
	// Single input dir is a misuse — caller should fall through to
	// the regular full-restore path.
	err := combine.Run(context.Background(), combine.CombineOptions{
		PGCombineBackupPath: "/usr/bin/true", // anything non-empty bypasses discover
		InputDirs:           []string{t.TempDir()},
		OutputDir:           t.TempDir(),
	})
	if err == nil {
		t.Error("expected combine.bad_inputs for single dir")
	}
}

// TestRun_BinaryNotOnPath: empty PGCombineBackupPath + empty PATH
// surfaces preflight.pg_combinebackup_missing with the package-name
// suggestion.
func TestRun_BinaryNotOnPath(t *testing.T) {
	t.Setenv("PATH", "")
	err := combine.Run(context.Background(), combine.CombineOptions{
		InputDirs: []string{t.TempDir(), t.TempDir()},
		OutputDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected pg_combinebackup not on PATH")
	}
	var oerr *output.Error
	if !errors.As(err, &oerr) || oerr.Code != "preflight.pg_combinebackup_missing" {
		t.Errorf("expected preflight.pg_combinebackup_missing, got %v", err)
	}
	if oerr.Suggestion == nil || !strings.Contains(oerr.Suggestion.Human, "postgresql-client") {
		t.Errorf("suggestion should mention postgresql-client; got %+v", oerr.Suggestion)
	}
}

// TestDiscover skips if the binary isn't installed (the common CI
// case); when it is, the returned path is absolute.
func TestDiscover(t *testing.T) {
	p, err := combine.DiscoverPGCombineBackup()
	if err != nil {
		t.Skipf("pg_combinebackup not on PATH: %v", err)
	}
	if p == "" || p[0] != '/' {
		t.Errorf("expected absolute path, got %q", p)
	}
}

// TestDiscoverForMajor_EnvOverride covers the operator-pinned path
// via PG_COMBINEBACKUP_<MAJOR>. We use /bin/true as a stand-in since
// the function only needs to find a readable file at the given path
// for the env-override branch (the version check is skipped when the
// override is present — pinning is the operator's responsibility).
func TestDiscoverForMajor_EnvOverride(t *testing.T) {
	t.Setenv("PG_COMBINEBACKUP_17", "/bin/true")
	p, err := combine.DiscoverPGCombineBackupForMajor(17)
	if err != nil {
		t.Fatalf("env override: %v", err)
	}
	if p != "/bin/true" {
		t.Errorf("env override = %q, want /bin/true", p)
	}
}

// TestDiscoverForMajor_NotFound exercises the failure path so an
// operator running on a host without the matching major sees the
// structured error we want them to (the prior code silently used a
// wrong-major binary on PATH and corrupted the restore).
func TestDiscoverForMajor_NotFound(t *testing.T) {
	t.Setenv("PG_COMBINEBACKUP_99", "")
	t.Setenv("PATH", "")
	_, err := combine.DiscoverPGCombineBackupForMajor(99)
	if err == nil {
		t.Fatal("expected error for impossible major")
	}
	if !strings.Contains(err.Error(), "pg_combinebackup matching PG 99 not found") {
		t.Errorf("error should name the major; got %v", err)
	}
}

// TestDiscoverForMajor_PathFallback validates the third lookup tier:
// PATH binary, ONLY if its --version matches. We can't reliably mock
// a wrong-version pg_combinebackup, but if the host has a real one
// we check the success path here. Skips when no binary is found.
func TestDiscoverForMajor_PathFallback(t *testing.T) {
	hostBin, err := combine.DiscoverPGCombineBackup()
	if err != nil {
		t.Skipf("no pg_combinebackup on host: %v", err)
	}
	// Probe the host binary's major by invoking it once. If we
	// can't, skip — the test is host-specific.
	out, err := osExecCommand(hostBin, "--version")
	if err != nil {
		t.Skipf("--version: %v", err)
	}
	var major int
	if _, err := fmt.Sscanf(stripBeforePostgres(string(out)), "%d", &major); err != nil || major <= 0 {
		t.Skipf("could not parse major from %q", out)
	}
	// Force the env override + per-distro paths empty so the
	// function MUST fall through to PATH.
	t.Setenv(fmt.Sprintf("PG_COMBINEBACKUP_%d", major), "")
	p, err := combine.DiscoverPGCombineBackupForMajor(major)
	if err != nil {
		t.Fatalf("PATH fallback for major %d: %v", major, err)
	}
	if p == "" {
		t.Errorf("PATH fallback returned empty path")
	}
}

// osExecCommand is a tiny indirection so the test doesn't drag in
// os/exec at the file top and bloat the imports.
func osExecCommand(name string, args ...string) ([]byte, error) {
	return execLookerForTest(name, args...)
}

// TestBuild_SystemIdentifierMismatch: a chain whose links don't all share
// one cluster's system identifier must be refused up front (before any
// materialisation) — pg_combinebackup would otherwise reject it deep into
// the merge after the whole chain is staged.
func TestBuild_SystemIdentifierMismatch(t *testing.T) {
	sp, signer, verifier := newRepo(t)
	t0 := time.Now().UTC().Add(-2 * time.Hour)
	full := mkFull("db1.full.x", t0)
	inc := mkInc("db1.incremental_lsn.x", full.BackupID, t0.Add(time.Hour))
	inc.SystemIdentifier = "9999999999999999999" // DIFFERENT cluster
	commitManifest(t, sp, signer, full)
	commitManifest(t, sp, signer, inc)

	_, err := combine.Build(context.Background(), sp, "db1", inc.BackupID, verifier)
	if err == nil {
		t.Fatal("expected refusal for a cross-cluster chain")
	}
	if oe, ok := err.(*output.Error); !ok || oe.Code != "chain.system_identifier_mismatch" {
		t.Errorf("want chain.system_identifier_mismatch; got %v", err)
	}
}

// TestBuild_PGMajorMismatch: links spanning two PG majors are refused
// (pg_combinebackup is version-locked to a single major).
func TestBuild_PGMajorMismatch(t *testing.T) {
	sp, signer, verifier := newRepo(t)
	t0 := time.Now().UTC().Add(-2 * time.Hour)
	full := mkFull("db1.full.y", t0)
	full.PGVersion = 170000
	inc := mkInc("db1.incremental_lsn.y", full.BackupID, t0.Add(time.Hour))
	inc.PGVersion = 180000 // different major
	commitManifest(t, sp, signer, full)
	commitManifest(t, sp, signer, inc)

	_, err := combine.Build(context.Background(), sp, "db1", inc.BackupID, verifier)
	if err == nil {
		t.Fatal("expected refusal for a cross-major chain")
	}
	if oe, ok := err.(*output.Error); !ok || oe.Code != "chain.pg_major_mismatch" {
		t.Errorf("want chain.pg_major_mismatch; got %v", err)
	}
}

// TestBuild_NonIncrementalMiddleLink: a full backup that wrongly carries a
// ParentBackupID lands as a NON-anchor link; only the anchor may be full.
func TestBuild_NonIncrementalMiddleLink(t *testing.T) {
	sp, signer, verifier := newRepo(t)
	t0 := time.Now().UTC().Add(-3 * time.Hour)
	anchor := mkFull("db1.full.anchor", t0)
	mid := mkFull("db1.full.mid", t0.Add(time.Hour)) // type=full but...
	mid.ParentBackupID = anchor.BackupID             // ...wrongly parented
	leaf := mkInc("db1.incremental_lsn.leaf", mid.BackupID, t0.Add(2*time.Hour))
	commitManifest(t, sp, signer, anchor)
	commitManifest(t, sp, signer, mid)
	commitManifest(t, sp, signer, leaf)

	_, err := combine.Build(context.Background(), sp, "db1", leaf.BackupID, verifier)
	if err == nil {
		t.Fatal("expected refusal for a full backup in a non-anchor position")
	}
	if oe, ok := err.(*output.Error); !ok || oe.Code != "chain.non_incremental_link" {
		t.Errorf("want chain.non_incremental_link; got %v", err)
	}
}
