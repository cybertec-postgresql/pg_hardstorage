package repoaudit_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repoaudit"
)

// auditWorld is the test fixture: an init'd repo, signing keypair,
// and helpers to plant manifests of various flavours (encrypted /
// plaintext / different KEKs / different deployments).
type auditWorld struct {
	sp       storage.StoragePlugin
	store    *backup.ManifestStore
	signer   *backup.Signer
	verifier *backup.Verifier
	repoURL  string
	meta     *repo.Metadata
}

func setupAuditWorld(t *testing.T) *auditWorld {
	t.Helper()
	root := t.TempDir()
	repoURL := "file://" + root
	res, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL})
	if err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	return &auditWorld{
		sp:       sp,
		store:    backup.NewManifestStore(sp),
		signer:   signer,
		verifier: verifier,
		repoURL:  repoURL,
		meta:     &res.Metadata,
	}
}

// commitPlain plants an unencrypted manifest with the given
// (deployment, suffix, idx) shape. Returns the backup ID.
func (w *auditWorld) commitPlain(t *testing.T, deployment, suffix string, idx int) string {
	t.Helper()
	cas := casdefault.New(w.sp)
	body := []byte("plain-" + deployment + "-" + suffix)
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 4, 30, 12, idx, 0, 0, time.UTC)
	id := deployment + ".plain." + suffix + "." + ts.Format("20060102T150405Z")
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        ts,
		StoppedAt:        ts.Add(30 * time.Second),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{{
			Path: "data/" + id, Size: int64(len(body)), Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: int64(len(body))}},
		}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	return id
}

// commitEncrypted plants an encrypted manifest with the given KEK ref.
func (w *auditWorld) commitEncrypted(t *testing.T, deployment, suffix string, idx int, kek [encryption.KeyLen]byte, kekRef string) string {
	t.Helper()
	var dek [encryption.KeyLen]byte
	if _, err := rand.Read(dek[:]); err != nil {
		t.Fatal(err)
	}
	wrapped, err := encryption.Wrap(kek, dek)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := aesgcm.New(dek[:])
	if err != nil {
		t.Fatal(err)
	}
	cas := casdefault.NewEncrypted(w.sp, enc)
	body := []byte("enc-" + deployment + "-" + suffix)
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 4, 30, 13, idx, 0, 0, time.UTC)
	id := deployment + ".enc." + suffix + "." + ts.Format("20060102T150405Z")
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        ts,
		StoppedAt:        ts.Add(30 * time.Second),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Encryption: &backup.EncryptionInfo{
			Scheme:          "aes-256-gcm",
			KEKRef:          kekRef,
			WrappedDEK:      base64.StdEncoding.EncodeToString(wrapped),
			EnvelopeVersion: 1,
		},
		Files: []backup.FileEntry{{
			Path: "data/" + id, Size: int64(len(body)), Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: int64(len(body))}},
		}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	return id
}

func mkKEK(t *testing.T) [encryption.KeyLen]byte {
	t.Helper()
	var k [encryption.KeyLen]byte
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatal(err)
	}
	return k
}

// TestAudit_EmptyRepo: a freshly-initialised repo with no
// deployments produces an empty Deployments slice and zero rollups.
func TestAudit_EmptyRepo(t *testing.T) {
	w := setupAuditWorld(t)
	rep, err := repoaudit.Audit(context.Background(), w.sp, w.meta, w.repoURL, repoaudit.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if rep.Schema != repoaudit.ReportSchema {
		t.Errorf("Schema = %q", rep.Schema)
	}
	if rep.URL != w.repoURL {
		t.Errorf("URL = %q", rep.URL)
	}
	if len(rep.Deployments) != 0 {
		t.Errorf("Deployments = %d, want 0", len(rep.Deployments))
	}
	if len(rep.KEKRefs) != 0 {
		t.Errorf("KEKRefs = %d, want 0 (no manifests)", len(rep.KEKRefs))
	}
	if rep.Repo == nil || rep.Repo.ID == "" {
		t.Errorf("Repo summary missing: %+v", rep.Repo)
	}
	if rep.DurationMS < 0 {
		t.Errorf("DurationMS = %d", rep.DurationMS)
	}
}

// TestAudit_SingleDeployment_Plain: one deployment, two plain
// manifests → posture=plaintext, no KEK refs.
func TestAudit_SingleDeployment_Plain(t *testing.T) {
	w := setupAuditWorld(t)
	w.commitPlain(t, "db1", "a", 0)
	w.commitPlain(t, "db1", "b", 1)

	rep, err := repoaudit.Audit(context.Background(), w.sp, w.meta, w.repoURL, repoaudit.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(rep.Deployments) != 1 {
		t.Fatalf("Deployments = %d, want 1", len(rep.Deployments))
	}
	d := rep.Deployments[0]
	if d.Name != "db1" {
		t.Errorf("Name = %q", d.Name)
	}
	if d.Active != 2 {
		t.Errorf("Active = %d, want 2", d.Active)
	}
	if d.Tombstoned != 0 || d.Held != 0 || d.SignatureFailed != 0 {
		t.Errorf("non-zero in unexpected counters: %+v", d)
	}
	if d.EncryptionPosture != "plaintext" {
		t.Errorf("EncryptionPosture = %q, want plaintext", d.EncryptionPosture)
	}
	if len(d.KEKRefs) != 0 {
		t.Errorf("KEKRefs = %v, want empty", d.KEKRefs)
	}
	if d.Latest == nil || d.Latest.Encrypted {
		t.Errorf("Latest = %+v", d.Latest)
	}
	if d.OldestStoppedAt.IsZero() || d.NewestStoppedAt.IsZero() {
		t.Errorf("StoppedAt brackets missing: %+v", d)
	}
	if d.NewestStoppedAt.Before(d.OldestStoppedAt) {
		t.Errorf("Newest before Oldest: %+v", d)
	}
}

// TestAudit_MultiDeployment_Encrypted: three deployments with
// different KEK refs → fleet rollup shows the breakdown.
func TestAudit_MultiDeployment_Encrypted(t *testing.T) {
	w := setupAuditWorld(t)
	kekA := mkKEK(t)
	kekB := mkKEK(t)
	w.commitEncrypted(t, "db1", "a", 1, kekA, "tenant-a:v1")
	w.commitEncrypted(t, "db1", "b", 2, kekA, "tenant-a:v1")
	w.commitEncrypted(t, "db2", "c", 3, kekB, "tenant-b:v1")
	w.commitPlain(t, "db3", "x", 4)

	rep, err := repoaudit.Audit(context.Background(), w.sp, w.meta, w.repoURL, repoaudit.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(rep.Deployments) != 3 {
		t.Fatalf("Deployments = %d, want 3", len(rep.Deployments))
	}
	// Fleet rollup: tenant-a:v1 = 2, tenant-b:v1 = 1, <unencrypted> = 1
	got := map[string]int{}
	for _, k := range rep.KEKRefs {
		got[k.KEKRef] = k.ManifestCount
	}
	want := map[string]int{
		"tenant-a:v1":   2,
		"tenant-b:v1":   1,
		"<unencrypted>": 1,
	}
	for ref, n := range want {
		if got[ref] != n {
			t.Errorf("KEKRef[%q] = %d, want %d (got=%v)", ref, got[ref], n, got)
		}
	}
}

// TestAudit_SchemaDistribution: all manifests share the same schema,
// so the rollup is a singleton row.
func TestAudit_SchemaDistribution(t *testing.T) {
	w := setupAuditWorld(t)
	w.commitPlain(t, "db1", "a", 0)
	w.commitPlain(t, "db1", "b", 1)
	w.commitPlain(t, "db2", "c", 2)
	rep, err := repoaudit.Audit(context.Background(), w.sp, w.meta, w.repoURL, repoaudit.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(rep.SchemaVersions) != 1 {
		t.Fatalf("SchemaVersions = %v, want 1 entry", rep.SchemaVersions)
	}
	if rep.SchemaVersions[0].ManifestCount != 3 {
		t.Errorf("count = %d, want 3", rep.SchemaVersions[0].ManifestCount)
	}
	if rep.SchemaVersions[0].Schema != backup.Schema {
		t.Errorf("Schema = %q, want %q", rep.SchemaVersions[0].Schema, backup.Schema)
	}
}

// TestAudit_DeploymentFilter: scope to one deployment; fleet rollups
// still cover all deployments.
func TestAudit_DeploymentFilter(t *testing.T) {
	w := setupAuditWorld(t)
	w.commitPlain(t, "db1", "a", 0)
	w.commitPlain(t, "db2", "b", 1)
	rep, err := repoaudit.Audit(context.Background(), w.sp, w.meta, w.repoURL, repoaudit.Options{
		Verifier:         w.verifier,
		DeploymentFilter: "db1",
	})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(rep.Deployments) != 1 {
		t.Errorf("Deployments = %d, want 1 (filtered)", len(rep.Deployments))
	}
	if rep.Deployments[0].Name != "db1" {
		t.Errorf("Name = %q", rep.Deployments[0].Name)
	}
	// Fleet rollup unchanged: all 2 manifests counted.
	total := 0
	for _, k := range rep.KEKRefs {
		total += k.ManifestCount
	}
	if total != 2 {
		t.Errorf("Fleet KEKRefs total = %d, want 2 (filter must not affect rollup)", total)
	}
	if rep.DeploymentFilter != "db1" {
		t.Errorf("DeploymentFilter = %q", rep.DeploymentFilter)
	}
}

// TestAudit_TombstonedCount: a soft-deleted manifest counts as
// tombstoned, not active.
func TestAudit_TombstonedCount(t *testing.T) {
	w := setupAuditWorld(t)
	a := w.commitPlain(t, "db1", "alive", 0)
	dead := w.commitPlain(t, "db1", "dead", 1)
	if err := w.store.SoftDelete(context.Background(), "db1", dead, "test", "test"); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	rep, err := repoaudit.Audit(context.Background(), w.sp, w.meta, w.repoURL, repoaudit.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(rep.Deployments) != 1 {
		t.Fatalf("Deployments = %d", len(rep.Deployments))
	}
	d := rep.Deployments[0]
	if d.Active != 1 {
		t.Errorf("Active = %d, want 1 (only %s)", d.Active, a)
	}
	if d.Tombstoned != 1 {
		t.Errorf("Tombstoned = %d, want 1 (%s)", d.Tombstoned, dead)
	}
}

// TestAudit_HeldCount: an active hold counts toward Held; an
// expired hold counts toward HeldExpired.
func TestAudit_HeldCount(t *testing.T) {
	w := setupAuditWorld(t)
	idActive := w.commitPlain(t, "db1", "active-hold", 0)
	idExpired := w.commitPlain(t, "db1", "expired-hold", 1)

	if err := w.store.PutHold(context.Background(), "db1", idActive, "ops", "indefinite"); err != nil {
		t.Fatalf("PutHold active: %v", err)
	}
	past := time.Now().Add(-1 * time.Hour)
	if err := w.store.PutHoldUntil(context.Background(), "db1", idExpired, "ops", "past", past); err != nil {
		t.Fatalf("PutHoldUntil expired: %v", err)
	}

	rep, err := repoaudit.Audit(context.Background(), w.sp, w.meta, w.repoURL, repoaudit.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	d := rep.Deployments[0]
	if d.Held != 1 {
		t.Errorf("Held = %d, want 1", d.Held)
	}
	if d.HeldExpired != 1 {
		t.Errorf("HeldExpired = %d, want 1", d.HeldExpired)
	}
}

// TestAudit_LatestBackup_Tracked: with multiple backups, Latest
// reflects the newest StoppedAt.
func TestAudit_LatestBackup_Tracked(t *testing.T) {
	w := setupAuditWorld(t)
	w.commitPlain(t, "db1", "first", 0)
	expectLatest := w.commitPlain(t, "db1", "newest", 5) // idx=5 → later StoppedAt

	rep, err := repoaudit.Audit(context.Background(), w.sp, w.meta, w.repoURL, repoaudit.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	d := rep.Deployments[0]
	if d.Latest == nil {
		t.Fatal("Latest is nil")
	}
	if d.Latest.BackupID != expectLatest {
		t.Errorf("Latest.BackupID = %q, want %q", d.Latest.BackupID, expectLatest)
	}
}

// TestAudit_StorageSection: --skip-storage suppresses the section;
// default includes it.
func TestAudit_StorageSection(t *testing.T) {
	w := setupAuditWorld(t)
	w.commitPlain(t, "db1", "a", 0)

	// Default: storage included.
	rep, err := repoaudit.Audit(context.Background(), w.sp, w.meta, w.repoURL, repoaudit.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if rep.Storage == nil {
		t.Fatal("Storage = nil; expected by default")
	}
	if rep.Storage.TotalObjects == 0 {
		t.Errorf("TotalObjects = 0; expected > 0")
	}

	// SkipStorage: section absent.
	rep, err = repoaudit.Audit(context.Background(), w.sp, w.meta, w.repoURL, repoaudit.Options{
		Verifier:    w.verifier,
		SkipStorage: true,
	})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if rep.Storage != nil {
		t.Errorf("Storage = %+v; want nil with SkipStorage", rep.Storage)
	}
}

// TestAudit_Validation: nil sp / Verifier surface clear errors.
func TestAudit_Validation(t *testing.T) {
	w := setupAuditWorld(t)
	if _, err := repoaudit.Audit(context.Background(), nil, w.meta, w.repoURL, repoaudit.Options{
		Verifier: w.verifier,
	}); err == nil {
		t.Error("nil sp should error")
	}
	if _, err := repoaudit.Audit(context.Background(), w.sp, w.meta, w.repoURL, repoaudit.Options{}); err == nil {
		t.Error("nil Verifier should error")
	}
}

// TestAudit_RepoMetadata_Surfaced: the repo's WORM mode + ID are
// reflected in the report.
func TestAudit_RepoMetadata_Surfaced(t *testing.T) {
	root := t.TempDir()
	repoURL := "file://" + root
	worm, err := repo.MakeWORMPolicy("compliance", "30d")
	if err != nil {
		t.Fatal(err)
	}
	res, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL, WORM: worm})
	if err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	_, verifier := mustKeypair(priv, pub)

	rep, err := repoaudit.Audit(context.Background(), sp, &res.Metadata, repoURL, repoaudit.Options{
		Verifier: verifier,
	})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if rep.Repo == nil {
		t.Fatal("Repo = nil")
	}
	if rep.Repo.WORMMode != "compliance" {
		t.Errorf("WORMMode = %q, want compliance", rep.Repo.WORMMode)
	}
	if rep.Repo.WORMRetentionSeconds == 0 {
		t.Errorf("WORMRetentionSeconds = 0; expected positive")
	}
}

// TestAudit_SignatureFailed_Counted: a manifest signed by a
// different keypair shows up in SignatureFailed.
func TestAudit_SignatureFailed_Counted(t *testing.T) {
	w := setupAuditWorld(t)
	// Plant a manifest with the world's signer.
	w.commitPlain(t, "db1", "good", 0)

	// Plant a second manifest signed with an UNRELATED keypair.
	otherPriv, _, _ := backup.GenerateKeypair(rand.Reader)
	otherSigner, _ := backup.LoadSigner(otherPriv)
	cas := casdefault.New(w.sp)
	body := []byte("rogue")
	info, _ := cas.PutChunk(context.Background(), body)
	ts := time.Date(2026, 4, 30, 14, 0, 0, 0, time.UTC)
	id := "db1.rogue." + ts.Format("20060102T150405Z")
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        ts,
		StoppedAt:        ts.Add(30 * time.Second),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{{
			Path: "data/" + id, Size: int64(len(body)), Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: int64(len(body))}},
		}},
	}
	if err := w.store.Commit(context.Background(), m, otherSigner, backup.CommitOptions{}); err != nil {
		t.Fatalf("Commit (rogue): %v", err)
	}

	rep, err := repoaudit.Audit(context.Background(), w.sp, w.meta, w.repoURL, repoaudit.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	d := rep.Deployments[0]
	if d.Active != 1 {
		t.Errorf("Active = %d, want 1 (one good manifest)", d.Active)
	}
	if d.SignatureFailed != 1 {
		t.Errorf("SignatureFailed = %d, want 1", d.SignatureFailed)
	}
}

// TestAudit_EncryptionPosture: encrypted-only / plaintext-only /
// mixed deployments classify correctly.
func TestAudit_EncryptionPosture(t *testing.T) {
	w := setupAuditWorld(t)
	kek := mkKEK(t)
	w.commitEncrypted(t, "db-enc", "a", 0, kek, "tenant-a:v1")
	w.commitEncrypted(t, "db-enc", "b", 1, kek, "tenant-a:v1")
	w.commitPlain(t, "db-plain", "a", 2)
	w.commitPlain(t, "db-plain", "b", 3)
	w.commitEncrypted(t, "db-mixed", "a", 4, kek, "tenant-a:v1")
	w.commitPlain(t, "db-mixed", "b", 5)

	rep, err := repoaudit.Audit(context.Background(), w.sp, w.meta, w.repoURL, repoaudit.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	got := map[string]string{}
	for _, d := range rep.Deployments {
		got[d.Name] = d.EncryptionPosture
	}
	want := map[string]string{
		"db-enc":   "encrypted",
		"db-plain": "plaintext",
		"db-mixed": "mixed",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("posture[%s] = %q, want %q", name, got[name], w)
		}
	}
}

// TestAudit_SkipApprovals_AndChain: opt-out flags suppress the
// optional sections.
func TestAudit_SkipOptionalSections(t *testing.T) {
	w := setupAuditWorld(t)
	w.commitPlain(t, "db1", "a", 0)

	rep, err := repoaudit.Audit(context.Background(), w.sp, w.meta, w.repoURL, repoaudit.Options{
		Verifier:       w.verifier,
		SkipStorage:    true,
		SkipAuditChain: true,
		SkipApprovals:  true,
	})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if rep.Storage != nil {
		t.Errorf("Storage = %+v, want nil", rep.Storage)
	}
	if rep.Chain != nil {
		t.Errorf("Chain = %+v, want nil", rep.Chain)
	}
	if rep.Approvals != nil {
		t.Errorf("Approvals = %+v, want nil", rep.Approvals)
	}
}

// TestAudit_ContextCancellation: returns ctx.Err.
func TestAudit_ContextCancellation(t *testing.T) {
	w := setupAuditWorld(t)
	w.commitPlain(t, "db1", "a", 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := repoaudit.Audit(ctx, w.sp, w.meta, w.repoURL, repoaudit.Options{
		Verifier: w.verifier,
	})
	if err == nil {
		t.Error("expected ctx error")
	}
}

// mustKeypair is a small helper to keep the keypair-load boilerplate
// out of individual tests.
func mustKeypair(priv []byte, pub []byte) (*backup.Signer, *backup.Verifier) {
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	return signer, verifier
}
