package dsa_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/dsa"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// signerFromKey is the same minimal Signer used in
// integrity / threshold tests.
type signerFromKey struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func (s signerFromKey) Sign(payload []byte) []byte   { return ed25519.Sign(s.priv, payload) }
func (s signerFromKey) PublicKey() ed25519.PublicKey { return s.pub }

func mustKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// fixture brings up a fresh repo + manifest store with the
// operator's signing keystore.
type fixture struct {
	sp        storage.StoragePlugin
	manifests *backup.ManifestStore
	signer    *backup.Signer
	verifier  *backup.Verifier
	locator   *dsa.Locator
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	keyringDir := t.TempDir()
	signer, verifier, err := keystore.LoadOrGenerate(keyringDir)
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := t.TempDir()
	repoURL := "file://" + repoRoot
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(repoURL)
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	ms := backup.NewManifestStore(sp)
	return &fixture{
		sp:        sp,
		manifests: ms,
		signer:    signer,
		verifier:  verifier,
		locator:   dsa.NewLocator(ms, verifier),
	}
}

// commitTenantBackup commits a manifest belonging to (deployment, tenant)
// at a specific timestamp.  Encryption is set when kekRef != "".
func (f *fixture) commitTenantBackup(t *testing.T, deployment, tenant, kekRef string, at time.Time, suffix string) {
	t.Helper()
	files := []backup.FileEntry{
		{Path: "PG_VERSION", Size: 3, Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: repo.HashOf([]byte("17\n")), Offset: 0, Len: 3}}},
	}
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         deployment + ".full." + at.Format("20060102T150405Z") + "." + suffix,
		Deployment:       deployment,
		Tenant:           tenant,
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        at,
		StoppedAt:        at.Add(30 * time.Second),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:            files,
	}
	if kekRef != "" {
		m.Encryption = &backup.EncryptionInfo{
			Scheme:          "aes-256-gcm",
			KEKRef:          kekRef,
			WrappedDEK:      "deadbeef",
			EnvelopeVersion: 1,
		}
	}
	if err := f.manifests.Commit(context.Background(), m, f.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// ----- validation -----

func TestLocate_RequiresSubjectID(t *testing.T) {
	f := newFixture(t)
	_, err := f.locator.Locate(context.Background(), dsa.LocateOptions{
		Tenant: "T", Article: dsa.ArticleErasure,
	})
	if !errors.Is(err, dsa.ErrSubjectIDRequired) {
		t.Errorf("err = %v, want ErrSubjectIDRequired", err)
	}
}

func TestLocate_RequiresTenant(t *testing.T) {
	f := newFixture(t)
	_, err := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID: "user-1", Article: dsa.ArticleErasure,
	})
	if !errors.Is(err, dsa.ErrTenantRequired) {
		t.Errorf("err = %v, want ErrTenantRequired", err)
	}
}

func TestLocate_RequiresArticle(t *testing.T) {
	f := newFixture(t)
	_, err := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID: "user-1", Tenant: "T",
	})
	if !errors.Is(err, dsa.ErrArticleRequired) {
		t.Errorf("err = %v, want ErrArticleRequired", err)
	}
}

func TestLocate_InvalidArticle(t *testing.T) {
	f := newFixture(t)
	_, err := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID: "user-1", Tenant: "T", Article: "art_42_omg",
	})
	if !errors.Is(err, dsa.ErrInvalidArticle) {
		t.Errorf("err = %v, want ErrInvalidArticle", err)
	}
}

// ----- locate scenarios -----

func TestLocate_FiltersByTenant(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	// Two backups in tenant-a, one in tenant-b.
	f.commitTenantBackup(t, "db1", "tenant-a", "kms://acme/a", now.Add(-2*time.Hour), "1")
	f.commitTenantBackup(t, "db1", "tenant-a", "kms://acme/a", now.Add(-1*time.Hour), "2")
	f.commitTenantBackup(t, "db1", "tenant-b", "kms://acme/b", now, "3")

	r, err := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID: "user-42",
		Tenant:    "tenant-a",
		Article:   dsa.ArticleErasure,
		Note:      "GDPR Art. 17 #5023",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.ManifestsScanned != 3 {
		t.Errorf("scanned = %d, want 3", r.ManifestsScanned)
	}
	if r.ManifestsAffected != 2 {
		t.Errorf("affected = %d, want 2", r.ManifestsAffected)
	}
	if r.Tenant != "tenant-a" {
		t.Errorf("tenant = %q", r.Tenant)
	}
	// Subject ID hashed, not stored raw.
	if r.SubjectIDHash == "" {
		t.Errorf("subject hash empty")
	}
	if r.SubjectIDHash == "user-42" {
		t.Errorf("subject id stored raw, want hashed")
	}
	if len(r.AffectedBackups) != 2 {
		t.Errorf("affected backups = %d", len(r.AffectedBackups))
	}
	for _, ab := range r.AffectedBackups {
		if ab.Tenant != "tenant-a" {
			t.Errorf("affected backup has wrong tenant: %+v", ab)
		}
		if ab.KEKRef != "kms://acme/a" {
			t.Errorf("affected backup has wrong KEKRef: %+v", ab)
		}
		if !ab.Encrypted {
			t.Errorf("affected backup should be marked encrypted")
		}
	}
}

func TestLocate_TimeWindow(t *testing.T) {
	f := newFixture(t)
	t1 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	f.commitTenantBackup(t, "db1", "T", "k", t1, "1")
	f.commitTenantBackup(t, "db1", "T", "k", t2, "2")
	f.commitTenantBackup(t, "db1", "T", "k", t3, "3")

	r, err := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID:   "user",
		Tenant:      "T",
		Article:     dsa.ArticleAccess,
		WindowStart: time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.ManifestsAffected != 1 {
		t.Errorf("affected = %d, want 1 (only the May backup is in-window)", r.ManifestsAffected)
	}
	if r.WindowStart == nil || r.WindowEnd == nil {
		t.Errorf("window not preserved on report")
	}
}

func TestLocate_DeploymentScoped(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f.commitTenantBackup(t, "db1", "T", "k", now, "1")
	f.commitTenantBackup(t, "db2", "T", "k", now.Add(time.Hour), "2")

	r, err := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID: "user", Tenant: "T", Article: dsa.ArticleAccess,
		Deployment: "db2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.ManifestsAffected != 1 {
		t.Errorf("affected = %d, want 1 (db2 only)", r.ManifestsAffected)
	}
	if r.AffectedBackups[0].Deployment != "db2" {
		t.Errorf("deployment = %q", r.AffectedBackups[0].Deployment)
	}
}

func TestLocate_NoMatches(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f.commitTenantBackup(t, "db1", "T-other", "k", now, "1")

	r, err := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID: "user", Tenant: "T-missing", Article: dsa.ArticleErasure,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.ManifestsAffected != 0 || len(r.AffectedBackups) != 0 {
		t.Errorf("expected zero affected, got: %+v", r)
	}
	// Even on zero matches we still produce an Article-17 action plan.
	if len(r.SuggestedActions) == 0 {
		t.Errorf("expected suggested actions")
	}
}

func TestLocate_DeploymentRollupSorted(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f.commitTenantBackup(t, "zeta", "T", "k", now, "1")
	f.commitTenantBackup(t, "alpha", "T", "k", now, "2")
	f.commitTenantBackup(t, "alpha", "T", "k", now.Add(time.Hour), "3")
	f.commitTenantBackup(t, "mu", "T", "k", now.Add(2*time.Hour), "4")

	r, err := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID: "user", Tenant: "T", Article: dsa.ArticleErasure,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.DeploymentsAffected != 3 {
		t.Errorf("deployments_affected = %d, want 3", r.DeploymentsAffected)
	}
	want := []string{"alpha", "mu", "zeta"}
	for i, d := range r.Deployments {
		if d.Deployment != want[i] {
			t.Errorf("Deployments[%d] = %q, want %q", i, d.Deployment, want[i])
		}
	}
	// alpha had two backups; verify both surfaced.
	if r.Deployments[0].BackupCount != 2 {
		t.Errorf("alpha backup_count = %d, want 2", r.Deployments[0].BackupCount)
	}
}

func TestLocate_AffectedBackupsSortedOldestFirst(t *testing.T) {
	f := newFixture(t)
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f.commitTenantBackup(t, "db1", "T", "k", t0.Add(2*time.Hour), "third")
	f.commitTenantBackup(t, "db1", "T", "k", t0, "first")
	f.commitTenantBackup(t, "db1", "T", "k", t0.Add(time.Hour), "second")

	r, err := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID: "u", Tenant: "T", Article: dsa.ArticleAccess,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.AffectedBackups) != 3 {
		t.Fatalf("len = %d, want 3", len(r.AffectedBackups))
	}
	for i := 1; i < len(r.AffectedBackups); i++ {
		if r.AffectedBackups[i].StartedAt.Before(r.AffectedBackups[i-1].StartedAt) {
			t.Errorf("not oldest-first: %v", r.AffectedBackups)
		}
	}
}

// ----- suggested actions -----

func TestSuggestedActions_Article17_RecommendsKMSShred(t *testing.T) {
	f := newFixture(t)
	r, _ := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID: "u", Tenant: "tenant-a",
		Article: dsa.ArticleErasure,
	})
	if len(r.SuggestedActions) == 0 {
		t.Fatal("no suggested actions")
	}
	hasShred := false
	for _, a := range r.SuggestedActions {
		if a.Article == dsa.ArticleErasure &&
			a.Command == "pg_hardstorage kms shred --tenant tenant-a" {
			hasShred = true
		}
	}
	if !hasShred {
		t.Errorf("expected a kms-shred action for tenant-a in: %+v", r.SuggestedActions)
	}
}

func TestSuggestedActions_Article15_RecommendsPartialRestore(t *testing.T) {
	f := newFixture(t)
	r, _ := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID: "u", Tenant: "T", Article: dsa.ArticleAccess,
	})
	if len(r.SuggestedActions) == 0 {
		t.Fatal("no suggested actions")
	}
	if r.SuggestedActions[0].Article != dsa.ArticleAccess {
		t.Errorf("first action article = %q", r.SuggestedActions[0].Article)
	}
}

// ----- sign / verify -----

func TestSignAndVerifyReport_RoundTrip(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f.commitTenantBackup(t, "db1", "T", "k", now, "1")
	r, err := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID: "u", Tenant: "T", Article: dsa.ArticleErasure,
	})
	if err != nil {
		t.Fatal(err)
	}

	pub, priv := mustKeypair(t)
	if err := dsa.SignReport(r, signerFromKey{pub: pub, priv: priv}); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if r.Signature == "" || r.BodyHash == "" || r.PublicKeyFingerprint == "" {
		t.Errorf("signing fields not populated")
	}
	if err := dsa.VerifyReport(r, &dsa.SingleKeyResolver{Key: pub}); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestVerifyReport_TamperedTenant(t *testing.T) {
	f := newFixture(t)
	r, _ := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID: "u", Tenant: "T", Article: dsa.ArticleErasure,
	})
	pub, priv := mustKeypair(t)
	_ = dsa.SignReport(r, signerFromKey{pub: pub, priv: priv})
	r.Tenant = "different-tenant"
	if err := dsa.VerifyReport(r, &dsa.SingleKeyResolver{Key: pub}); err == nil {
		t.Errorf("expected verify failure after tampering Tenant")
	}
}

func TestVerifyReport_TamperedAffectedBackups(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f.commitTenantBackup(t, "db1", "T", "k", now, "1")
	r, _ := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID: "u", Tenant: "T", Article: dsa.ArticleErasure,
	})
	pub, priv := mustKeypair(t)
	_ = dsa.SignReport(r, signerFromKey{pub: pub, priv: priv})

	// Drop the affected backup.  The signature commits to the
	// affected-backup digest, so verification must reject.
	r.AffectedBackups = nil
	if err := dsa.VerifyReport(r, &dsa.SingleKeyResolver{Key: pub}); err == nil {
		t.Errorf("expected verify failure after dropping AffectedBackups")
	}
}

func TestVerifyReport_WrongKey(t *testing.T) {
	f := newFixture(t)
	r, _ := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID: "u", Tenant: "T", Article: dsa.ArticleErasure,
	})
	pub, priv := mustKeypair(t)
	_ = dsa.SignReport(r, signerFromKey{pub: pub, priv: priv})

	otherPub, _ := mustKeypair(t)
	if err := dsa.VerifyReport(r, &dsa.SingleKeyResolver{Key: otherPub}); !errors.Is(err, dsa.ErrSignatureInvalid) {
		t.Errorf("err = %v, want ErrSignatureInvalid", err)
	}
}

// ----- privacy -----

func TestSubjectID_NotStoredRaw(t *testing.T) {
	f := newFixture(t)
	r, err := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID: "personal-info-leakage-vector",
		Tenant:    "T", Article: dsa.ArticleErasure,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := marshalReport(r)
	if contains(body, "personal-info-leakage-vector") {
		t.Errorf("raw subject_id leaked into report body:\n%s", body)
	}
}

// ----- store round-trip -----

func TestReportStore_RoundTrip(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f.commitTenantBackup(t, "db1", "T", "k", now, "1")
	r, _ := f.locator.Locate(context.Background(), dsa.LocateOptions{
		SubjectID: "u", Tenant: "T", Article: dsa.ArticleErasure,
	})
	pub, priv := mustKeypair(t)
	_ = dsa.SignReport(r, signerFromKey{pub: pub, priv: priv})

	store := dsa.NewReportStore(f.sp)
	if err := store.Put(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != r.ID || got.Tenant != r.Tenant {
		t.Errorf("round-trip drift: %+v vs %+v", got, r)
	}
	if err := dsa.VerifyReport(got, &dsa.SingleKeyResolver{Key: pub}); err != nil {
		t.Errorf("Verify after read-back: %v", err)
	}
}

func TestReportStore_GetMissing(t *testing.T) {
	f := newFixture(t)
	store := dsa.NewReportStore(f.sp)
	_, err := store.Get(context.Background(), "ghost")
	if !errors.Is(err, dsa.ErrReportNotFound) {
		t.Errorf("err = %v, want ErrReportNotFound", err)
	}
}

func TestReportStore_ListFiltering(t *testing.T) {
	f := newFixture(t)
	pub, priv := mustKeypair(t)
	store := dsa.NewReportStore(f.sp)

	// Plant a tenant-a backup so we have something to find.
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f.commitTenantBackup(t, "db1", "tenant-a", "k", now, "1")

	// Three reports across tenants/articles.
	for i, opts := range []dsa.LocateOptions{
		{SubjectID: "user-1", Tenant: "tenant-a", Article: dsa.ArticleErasure},
		{SubjectID: "user-2", Tenant: "tenant-a", Article: dsa.ArticleAccess},
		{SubjectID: "user-3", Tenant: "tenant-b", Article: dsa.ArticleErasure},
	} {
		// Use Now to space the reports apart so IDs are distinct.
		ts := now.Add(time.Duration(i) * time.Hour)
		opts.Now = func() time.Time { return ts }
		r, err := f.locator.Locate(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		_ = dsa.SignReport(r, signerFromKey{pub: pub, priv: priv})
		if err := store.Put(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}

	all, err := store.List(context.Background(), dsa.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("len = %d, want 3", len(all))
	}
	// Newest first.
	if !all[0].GeneratedAt.After(all[1].GeneratedAt) {
		t.Errorf("not newest-first")
	}

	// Filter by tenant.
	scoped, _ := store.List(context.Background(), dsa.ListFilter{Tenant: "tenant-a"})
	if len(scoped) != 2 {
		t.Errorf("tenant filter: len = %d, want 2", len(scoped))
	}

	// Filter by article.
	scoped, _ = store.List(context.Background(), dsa.ListFilter{Article: dsa.ArticleErasure})
	if len(scoped) != 2 {
		t.Errorf("article filter: len = %d, want 2", len(scoped))
	}

	// Filter by subject hash.
	hash := dsa.HashSubjectIDForFilter("user-2")
	scoped, _ = store.List(context.Background(), dsa.ListFilter{SubjectIDHash: hash})
	if len(scoped) != 1 {
		t.Errorf("subject-hash filter: len = %d, want 1", len(scoped))
	}
}

// ----- helpers -----

func marshalReport(r *dsa.Report) (string, error) {
	body, err := jsonMarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		stringContains(haystack, needle)
}

// inlined to avoid pulling in strings just for this one test.
func stringContains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// jsonMarshalIndent isolates the encoding/json import to one
// helper.
func jsonMarshalIndent(v any, prefix, indent string) ([]byte, error) {
	return json.MarshalIndent(v, prefix, indent)
}
