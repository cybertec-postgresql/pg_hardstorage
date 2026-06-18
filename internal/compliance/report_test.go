package compliance_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/compliance"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// complianceWorld is the test fixture: an init'd repo, a signing
// keypair, and helpers to plant manifests + audit events.
type complianceWorld struct {
	sp       storage.StoragePlugin
	store    *backup.ManifestStore
	audit    *audit.Store
	signer   *backup.Signer
	verifier *backup.Verifier
	repoURL  string
	meta     *repo.Metadata
}

func setupWorld(t *testing.T) *complianceWorld {
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
	return &complianceWorld{
		sp:       sp,
		store:    backup.NewManifestStore(sp),
		audit:    audit.NewStore(sp),
		signer:   signer,
		verifier: verifier,
		repoURL:  repoURL,
		meta:     &res.Metadata,
	}
}

func (w *complianceWorld) commitBackup(t *testing.T, deployment, suffix string, stoppedAt time.Time, encrypted bool, btype backup.BackupType) string {
	t.Helper()
	var (
		cas  *repo.CAS
		body []byte
		enc  *backup.EncryptionInfo
	)
	body = []byte("payload-" + deployment + "-" + suffix)
	if encrypted {
		var dek [encryption.KeyLen]byte
		if _, err := rand.Read(dek[:]); err != nil {
			t.Fatal(err)
		}
		var kek [encryption.KeyLen]byte
		if _, err := rand.Read(kek[:]); err != nil {
			t.Fatal(err)
		}
		wrapped, err := encryption.Wrap(kek, dek)
		if err != nil {
			t.Fatal(err)
		}
		aead, err := aesgcm.New(dek[:])
		if err != nil {
			t.Fatal(err)
		}
		cas = casdefault.NewEncrypted(w.sp, aead)
		enc = &backup.EncryptionInfo{
			Scheme:          "aes-256-gcm",
			KEKRef:          "test:v1",
			WrappedDEK:      base64.StdEncoding.EncodeToString(wrapped),
			EnvelopeVersion: 1,
		}
	} else {
		cas = casdefault.New(w.sp)
	}
	info, err := cas.PutChunk(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	id := deployment + "." + string(btype) + "." + suffix + "." + stoppedAt.Format("20060102T150405Z")
	// Incrementals are committed anchorless (empty parent): these
	// report tests only count backups by type, and Commit now refuses
	// an incremental whose parent isn't live. An empty parent is
	// accepted by Validate and keeps the per-type counts stable
	// without having to plant (and count) a separate parent full.
	parent := ""
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             btype,
		ParentBackupID:   parent,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        stoppedAt.Add(-30 * time.Second),
		StoppedAt:        stoppedAt,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Encryption:       enc,
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

func (w *complianceWorld) appendAudit(t *testing.T, action, deployment string, at time.Time, body map[string]any) {
	t.Helper()
	ev := &audit.Event{
		Action:    action,
		Subject:   audit.Subject{Deployment: deployment},
		Timestamp: at,
		Body:      body,
	}
	if err := w.audit.Append(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
}

// TestGenerate_EmptyRepo: a fresh repo produces a clean report with
// zero counts across every section.
func TestGenerate_EmptyRepo(t *testing.T) {
	w := setupWorld(t)

	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if rep.Schema != compliance.ReportSchema {
		t.Errorf("Schema = %q", rep.Schema)
	}
	if rep.Backups == nil || rep.Backups.TotalCommitted != 0 {
		t.Errorf("expected zero backups; got %+v", rep.Backups)
	}
	if rep.Encryption == nil || rep.Encryption.EncryptedCount != 0 {
		t.Errorf("expected zero encryption; got %+v", rep.Encryption)
	}
	if rep.Chain == nil {
		t.Errorf("Chain section should always be present")
	}
}

// TestGenerate_DefaultWindow: when Since/Until are unset, the
// report uses (now-30d, now).
func TestGenerate_DefaultWindow(t *testing.T) {
	w := setupWorld(t)
	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	span := rep.Until.Sub(rep.Since)
	if want := compliance.DefaultWindow; span < want-time.Second || span > want+time.Second {
		t.Errorf("default window = %s, want ~30d", span)
	}
}

// TestGenerate_BackupSection_PerType: full + incremental counts
// roll up correctly.
func TestGenerate_BackupSection_PerType(t *testing.T) {
	w := setupWorld(t)
	now := time.Now().UTC()
	w.commitBackup(t, "db1", "a", now.Add(-1*time.Hour), false, backup.BackupTypeFull)
	w.commitBackup(t, "db1", "b", now.Add(-30*time.Minute), false, backup.BackupTypeIncremental)
	w.commitBackup(t, "db1", "c", now.Add(-15*time.Minute), false, backup.BackupTypeIncremental)
	w.commitBackup(t, "db2", "d", now.Add(-2*time.Hour), false, backup.BackupTypeFull)

	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	b := rep.Backups
	if b == nil || b.TotalCommitted != 4 {
		t.Fatalf("TotalCommitted = %d, want 4", b.TotalCommitted)
	}
	if b.ByType["full"] != 2 {
		t.Errorf("full = %d, want 2", b.ByType["full"])
	}
	if b.ByType[string(backup.BackupTypeIncremental)] != 2 {
		t.Errorf("incremental = %d, want 2 (key=%q)",
			b.ByType[string(backup.BackupTypeIncremental)],
			backup.BackupTypeIncremental)
	}
	if len(b.ByDeployment) != 2 {
		t.Fatalf("deployments = %d, want 2", len(b.ByDeployment))
	}
	for _, d := range b.ByDeployment {
		if d.Deployment == "db1" {
			if d.BackupCount != 3 || d.FullCount != 1 || d.IncCount != 2 {
				t.Errorf("db1 row off: %+v", d)
			}
		}
	}
}

// TestGenerate_BackupWindow_Filters: backups outside the window
// don't count.
func TestGenerate_BackupWindow_Filters(t *testing.T) {
	w := setupWorld(t)
	now := time.Now().UTC()
	w.commitBackup(t, "db1", "in", now.Add(-1*time.Hour), false, backup.BackupTypeFull)
	w.commitBackup(t, "db1", "out", now.Add(-100*24*time.Hour), false, backup.BackupTypeFull)

	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
		Since:    now.Add(-7 * 24 * time.Hour),
		Until:    now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Backups.TotalCommitted != 1 {
		t.Errorf("TotalCommitted = %d, want 1 (only the in-window backup)", rep.Backups.TotalCommitted)
	}
}

// TestGenerate_EncryptionCoverage: the coverage % is correct for a
// mixed-encryption fleet.
func TestGenerate_EncryptionCoverage(t *testing.T) {
	w := setupWorld(t)
	now := time.Now().UTC()
	w.commitBackup(t, "db1", "enc", now.Add(-1*time.Hour), true, backup.BackupTypeFull)
	w.commitBackup(t, "db1", "enc2", now.Add(-30*time.Minute), true, backup.BackupTypeFull)
	w.commitBackup(t, "db2", "plain", now.Add(-2*time.Hour), false, backup.BackupTypeFull)

	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	e := rep.Encryption
	if e.EncryptedCount != 2 || e.UnencryptedCount != 1 {
		t.Errorf("counts off: %+v", e)
	}
	want := float64(2) * 100 / 3
	if e.CoveragePercent < want-0.001 || e.CoveragePercent > want+0.001 {
		t.Errorf("CoveragePercent = %v, want ~%v", e.CoveragePercent, want)
	}
	// One encrypted KEK ref ("test:v1"), and the plaintext doesn't
	// contribute to ByKEKRef (no Encryption block).
	if len(e.ByKEKRef) != 1 || e.ByKEKRef[0].KEKRef != "test:v1" || e.ByKEKRef[0].ManifestCount != 2 {
		t.Errorf("ByKEKRef = %+v", e.ByKEKRef)
	}
}

// TestGenerate_Verification_FromAuditEvents: verify.* audit events
// surface as VerificationSection rows.
func TestGenerate_Verification_FromAuditEvents(t *testing.T) {
	w := setupWorld(t)
	now := time.Now().UTC()
	w.appendAudit(t, "verify.run", "db1", now.Add(-1*time.Hour), map[string]any{"outcome": "ok"})
	w.appendAudit(t, "verify.run", "db1", now.Add(-30*time.Minute), map[string]any{"outcome": "ok"})
	w.appendAudit(t, "verify.run", "db2", now.Add(-90*time.Minute), map[string]any{"outcome": "failed"})

	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := rep.Verification
	if v.TotalRuns != 3 {
		t.Errorf("TotalRuns = %d, want 3", v.TotalRuns)
	}
	if v.ByOutcome["ok"] != 2 || v.ByOutcome["failed"] != 1 {
		t.Errorf("ByOutcome = %v", v.ByOutcome)
	}
	if len(v.ByDeployment) != 2 {
		t.Errorf("ByDeployment = %v", v.ByDeployment)
	}
}

// TestGenerate_KEKLifecycle_FromAuditEvents: kms.rotate +
// kms.shred events surface in the lifecycle section.
func TestGenerate_KEKLifecycle_FromAuditEvents(t *testing.T) {
	w := setupWorld(t)
	now := time.Now().UTC()
	w.appendAudit(t, "kms.rotate", "", now.Add(-1*time.Hour), map[string]any{
		"old_kek_ref": "tenant:v1",
		"new_kek_ref": "tenant:v2",
		"rotated":     5,
	})
	w.appendAudit(t, "kms.shred", "", now.Add(-30*time.Minute), map[string]any{"tenant": "old"})

	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	k := rep.KEKLifecycle
	if k.RotationsAttempted != 1 || k.RotationsSucceeded != 1 {
		t.Errorf("rotations off: %+v", k)
	}
	if k.ShredsAttempted != 1 {
		t.Errorf("shreds = %d, want 1", k.ShredsAttempted)
	}
	if len(k.Events) != 2 {
		t.Errorf("Events = %d, want 2", len(k.Events))
	}
	// First (newest-first ordering) is shred; second is rotate.
	if k.Events[0].Action != "kms.shred" {
		t.Errorf("Events[0] = %+v, want kms.shred first", k.Events[0])
	}
	rotateEv := k.Events[1]
	if rotateEv.OldRef != "tenant:v1" || rotateEv.NewRef != "tenant:v2" {
		t.Errorf("rotate event refs off: %+v", rotateEv)
	}
}

// TestGenerate_DestructiveOps_Counted: backup.delete / kms.shred /
// repo.gc / repo.wipe / repo.set_mode count toward
// ApprovalSection.DestructiveOps.
func TestGenerate_DestructiveOps_Counted(t *testing.T) {
	w := setupWorld(t)
	now := time.Now().UTC()
	w.appendAudit(t, "backup.delete", "db1", now.Add(-1*time.Hour), nil)
	w.appendAudit(t, "kms.shred", "", now.Add(-2*time.Hour), nil)
	w.appendAudit(t, "repo.gc", "", now.Add(-3*time.Hour), nil)
	w.appendAudit(t, "repo.set_mode", "", now.Add(-4*time.Hour), nil)
	w.appendAudit(t, "verify.run", "db1", now.Add(-30*time.Minute), nil) // NOT destructive

	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	a := rep.Approvals
	if a.DestructiveOps != 4 {
		t.Errorf("DestructiveOps = %d, want 4", a.DestructiveOps)
	}
	if a.DestructiveByOp["backup.delete"] != 1 ||
		a.DestructiveByOp["kms.shred"] != 1 ||
		a.DestructiveByOp["repo.gc"] != 1 ||
		a.DestructiveByOp["repo.set_mode"] != 1 {
		t.Errorf("DestructiveByOp off: %v", a.DestructiveByOp)
	}
}

// TestGenerate_HoldLifecycle: hold.add/remove/expire events count.
func TestGenerate_HoldLifecycle(t *testing.T) {
	w := setupWorld(t)
	now := time.Now().UTC()
	w.appendAudit(t, "hold.add", "db1", now.Add(-1*time.Hour), nil)
	w.appendAudit(t, "hold.add", "db1", now.Add(-2*time.Hour), nil)
	w.appendAudit(t, "hold.remove", "db1", now.Add(-30*time.Minute), nil)
	w.appendAudit(t, "hold.purge_expired", "db1", now.Add(-15*time.Minute), nil)

	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := rep.Holds
	if h.HoldsAdded != 2 || h.HoldsRemoved != 1 || h.HoldsExpired != 1 {
		t.Errorf("hold counts off: %+v", h)
	}
}

// TestGenerate_DeploymentFilter: only the named deployment shows
// up in windowed sections; repo-wide sections still cover all.
func TestGenerate_DeploymentFilter(t *testing.T) {
	w := setupWorld(t)
	now := time.Now().UTC()
	w.commitBackup(t, "db1", "a", now.Add(-1*time.Hour), false, backup.BackupTypeFull)
	w.commitBackup(t, "db2", "b", now.Add(-1*time.Hour), false, backup.BackupTypeFull)

	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier:         w.verifier,
		DeploymentFilter: "db1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Backups.TotalCommitted != 1 {
		t.Errorf("Backups = %d, want 1 (filtered)", rep.Backups.TotalCommitted)
	}
	if rep.DeploymentFilter != "db1" {
		t.Errorf("DeploymentFilter = %q", rep.DeploymentFilter)
	}
}

// TestGenerate_SkipFlags: every Skip flag suppresses its section.
func TestGenerate_SkipFlags(t *testing.T) {
	w := setupWorld(t)
	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier:         w.verifier,
		SkipBackups:      true,
		SkipEncryption:   true,
		SkipVerification: true,
		SkipKEKLifecycle: true,
		SkipApprovals:    true,
		SkipHolds:        true,
		SkipReplicas:     true,
		SkipChain:        true,
		SkipWORM:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Backups != nil || rep.Encryption != nil || rep.Verification != nil ||
		rep.KEKLifecycle != nil || rep.Approvals != nil || rep.Holds != nil ||
		rep.Replicas != nil || rep.Chain != nil || rep.WORM != nil {
		t.Errorf("expected all sections nil; got %+v", rep)
	}
}

// TestGenerate_WORMSurfaced: a WORM-configured repo surfaces the
// WORMSection.Active=true with mode + retention.
func TestGenerate_WORMSurfaced(t *testing.T) {
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
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	_ = signer

	rep, err := compliance.Generate(context.Background(), sp, &res.Metadata, repoURL, compliance.Options{
		Verifier: verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.WORM == nil || !rep.WORM.Active {
		t.Errorf("WORM section missing or inactive: %+v", rep.WORM)
	}
	if rep.WORM.Mode != "compliance" {
		t.Errorf("WORM.Mode = %q", rep.WORM.Mode)
	}
}

// TestGenerate_SchemeUsed_Aggregated: the encryption section
// surfaces every scheme observed.
func TestGenerate_SchemeUsed_Aggregated(t *testing.T) {
	w := setupWorld(t)
	now := time.Now().UTC()
	w.commitBackup(t, "db1", "a", now.Add(-1*time.Hour), true, backup.BackupTypeFull)
	w.commitBackup(t, "db1", "b", now.Add(-30*time.Minute), true, backup.BackupTypeFull)

	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(rep.Encryption.SchemesUsed, "aes-256-gcm") {
		t.Errorf("SchemesUsed missing aes-256-gcm: %v", rep.Encryption.SchemesUsed)
	}
}

// TestGenerate_Validation: programmer-error guards.
func TestGenerate_Validation(t *testing.T) {
	w := setupWorld(t)
	if _, err := compliance.Generate(context.Background(), nil, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	}); err == nil {
		t.Error("nil sp must error")
	}
	if _, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{}); err == nil {
		t.Error("nil verifier must error")
	}
	now := time.Now().UTC()
	if _, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
		Since:    now,
		Until:    now.Add(-1 * time.Hour),
	}); err == nil {
		t.Error("Since>=Until must error")
	}
}

// TestGenerate_ChainSection_Always: skipChainVerify off → verify
// fields populated; on → counts only.
func TestGenerate_ChainSection_VerifyOpt(t *testing.T) {
	w := setupWorld(t)
	w.appendAudit(t, "info.test", "db1", time.Now().UTC().Add(-1*time.Minute), nil)

	withVerify, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if withVerify.Chain == nil || withVerify.Chain.VerifyEventsChecked == 0 {
		t.Errorf("VerifyEventsChecked should be > 0 (chain has 1 event): %+v", withVerify.Chain)
	}

	noVerify, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier:        w.verifier,
		SkipChainVerify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if noVerify.Chain == nil {
		t.Fatal("Chain section should still be present")
	}
	if noVerify.Chain.VerifyEventsChecked != 0 {
		t.Errorf("VerifyEventsChecked = %d, want 0 (verify skipped)",
			noVerify.Chain.VerifyEventsChecked)
	}
}

// TestRenderMarkdown_HappyPath: the Markdown rendering surfaces
// every section's heading + the basic facts. We don't assert
// strict layout — only that the substantive bits are present.
func TestRenderMarkdown_HappyPath(t *testing.T) {
	w := setupWorld(t)
	now := time.Now().UTC()
	w.commitBackup(t, "db1", "a", now.Add(-1*time.Hour), true, backup.BackupTypeFull)
	w.appendAudit(t, "kms.rotate", "", now.Add(-30*time.Minute), map[string]any{
		"old_kek_ref": "test:v1", "new_kek_ref": "test:v2",
	})
	w.appendAudit(t, "hold.add", "db1", now.Add(-15*time.Minute), nil)

	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := compliance.RenderMarkdown(&sb, rep); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"# pg_hardstorage compliance report",
		"## Backup activity",
		"## Encryption coverage",
		"## Verification coverage",
		"## KEK lifecycle",
		"## Approval workflow",
		"## Holds",
		"## Replica completeness",
		"## Audit chain",
		"## WORM",
		"db1",
		"`test:v1`",
		"`test:v2`",
		"`hold.add`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Markdown missing %q in:\n%s", want, out)
		}
	}
}

// TestRenderMarkdown_AllSkipped: a report with every section
// skipped renders cleanly with "(skipped)" placeholders. No
// crashes; no garbage.
func TestRenderMarkdown_AllSkipped(t *testing.T) {
	w := setupWorld(t)
	rep, err := compliance.Generate(context.Background(), w.sp, w.meta, w.repoURL, compliance.Options{
		Verifier:         w.verifier,
		SkipBackups:      true,
		SkipEncryption:   true,
		SkipVerification: true,
		SkipKEKLifecycle: true,
		SkipApprovals:    true,
		SkipHolds:        true,
		SkipReplicas:     true,
		SkipChain:        true,
		SkipWORM:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := compliance.RenderMarkdown(&sb, rep); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if strings.Count(out, "(skipped)") < 7 {
		t.Errorf("expected several (skipped) entries; got:\n%s", out)
	}
}

// TestFormatPercent: a simple sanity check on the percentage helper.
func TestFormatPercent(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0.00%"},
		{50, "50.00%"},
		{99.5, "99.50%"},
		{100, "100.00%"},
	}
	for _, c := range cases {
		if got := compliance.FormatPercent(c.in); got != c.want {
			t.Errorf("FormatPercent(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
