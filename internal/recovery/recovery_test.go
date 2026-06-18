package recovery_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/recovery"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

type recoveryWorld struct {
	sp       storage.StoragePlugin
	store    *backup.ManifestStore
	signer   *backup.Signer
	verifier *backup.Verifier
	repoURL  string
}

func setupWorld(t *testing.T) *recoveryWorld {
	t.Helper()
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
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
	return &recoveryWorld{
		sp:       sp,
		store:    backup.NewManifestStore(sp),
		signer:   signer,
		verifier: verifier,
		repoURL:  repoURL,
	}
}

func (w *recoveryWorld) commitBackup(t *testing.T, deployment string, stoppedAt time.Time, bytes int64, encrypted bool, btype backup.BackupType, timeline uint32) string {
	t.Helper()
	var (
		cas *repo.CAS
		enc *backup.EncryptionInfo
	)
	body := []byte(strings.Repeat("x", 16))
	if encrypted {
		var dek, kek [encryption.KeyLen]byte
		_, _ = rand.Read(dek[:])
		_, _ = rand.Read(kek[:])
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
	id := deployment + "." + string(btype) + "." + stoppedAt.Format("20060102T150405.000Z")
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         id,
		Deployment:       deployment,
		Tenant:           "default",
		Type:             btype,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         timeline,
		StartedAt:        stoppedAt.Add(-30 * time.Second),
		StoppedAt:        stoppedAt,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Encryption:       enc,
		Files: []backup.FileEntry{{
			// Declared logical size = bytes; chunk Len matches so
			// Manifest.Validate's chunk-sum-equals-file-size invariant
			// holds. (The CAS body is a small fixed payload; recovery
			// reads manifest-reported size.)
			Path: "data/" + id, Size: bytes, Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: info.Hash, Offset: 0, Len: bytes}},
		}},
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	return id
}

// goodResolver returns a fixed key regardless of ref. Used when
// tests don't care about KEK matching.
func goodResolver() func(string) ([encryption.KeyLen]byte, error) {
	return func(string) ([encryption.KeyLen]byte, error) {
		var k [encryption.KeyLen]byte
		return k, nil
	}
}

// badResolver always errors. Useful for the kek-unreachable case.
func badResolver(msg string) func(string) ([encryption.KeyLen]byte, error) {
	return func(string) ([encryption.KeyLen]byte, error) {
		var k [encryption.KeyLen]byte
		return k, simpleError(msg)
	}
}

type simpleErr string

func (s simpleErr) Error() string { return string(s) }
func simpleError(s string) error  { return simpleErr(s) }

// TestReadiness_NoBackups: a fresh deployment surfaces NoBackups
// + a critical issue.
func TestReadiness_NoBackups(t *testing.T) {
	w := setupWorld(t)
	r, err := recovery.Readiness(context.Background(), w.sp, "db1", recovery.Options{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatalf("Readiness: %v", err)
	}
	if r.OverallStatus != recovery.StatusNoBackups {
		t.Errorf("Status = %q, want no_backups", r.OverallStatus)
	}
	if len(r.Issues) == 0 || r.Issues[0].Code != "recovery.no_backups" {
		t.Errorf("expected recovery.no_backups; got %+v", r.Issues)
	}
}

// TestReadiness_HappyPath_NoSLO: a deployment with one fresh
// backup, no SLO target → Status=ready (or close to it).
func TestReadiness_HappyPath_NoSLO(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitBackup(t, "db1", now.Add(-1*time.Hour), 1<<30, false, backup.BackupTypeFull, 1)

	r, err := recovery.Readiness(context.Background(), w.sp, "db1", recovery.Options{
		Verifier: w.verifier,
		Now:      now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.BackupCount != 1 {
		t.Errorf("BackupCount = %d, want 1", r.BackupCount)
	}
	if r.Latest == nil {
		t.Fatal("Latest missing")
	}
	if r.Latest.AgeSeconds < 3500 || r.Latest.AgeSeconds > 3700 {
		t.Errorf("AgeSeconds = %d, want ~3600", r.Latest.AgeSeconds)
	}
	// Status: depends on whether NoticeIssues fired (no replica is
	// a notice). With notice-only issues, status is still "ready"
	// per computeOverallStatus (only critical+warning bump it).
	if r.OverallStatus == recovery.StatusNotReady {
		t.Errorf("Status = %q, want ready or ready_with_warnings", r.OverallStatus)
	}
}

// TestReadiness_RPOMissed: backup older than RPOTarget → critical
// issue + Status=not_ready.
func TestReadiness_RPOMissed(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitBackup(t, "db1", now.Add(-25*time.Hour), 1<<30, false, backup.BackupTypeFull, 1)

	r, err := recovery.Readiness(context.Background(), w.sp, "db1", recovery.Options{
		Verifier:         w.verifier,
		Now:              now,
		RPOTargetSeconds: 24 * 3600, // 24h
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.RPO == nil {
		t.Fatal("RPO missing")
	}
	if r.RPO.Met {
		t.Errorf("RPO.Met = true; expected false (25h > 24h target)")
	}
	if r.OverallStatus != recovery.StatusNotReady {
		t.Errorf("Status = %q, want not_ready", r.OverallStatus)
	}
	found := false
	for _, i := range r.Issues {
		if i.Code == "recovery.rpo_missed" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected recovery.rpo_missed in Issues: %+v", r.Issues)
	}
}

// TestReadiness_RTOTargetMissed: large backup at low throughput →
// RTO miss → warning issue + Status=ready_with_warnings.
func TestReadiness_RTOTargetMissed(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// 100 GiB backup; 1 MiB/s throughput → ~100k seconds.
	w.commitBackup(t, "db1", now.Add(-1*time.Hour), 100<<30, false, backup.BackupTypeFull, 1)

	r, err := recovery.Readiness(context.Background(), w.sp, "db1", recovery.Options{
		Verifier:          w.verifier,
		Now:               now,
		AssumedThroughput: 1 << 20, // 1 MiB/s
		RTOTargetSeconds:  3600,    // 1 hour
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.RTO == nil {
		t.Fatal("RTO missing")
	}
	if r.RTO.Met {
		t.Errorf("RTO.Met = true; expected false (100k seconds > 1h)")
	}
	found := false
	for _, i := range r.Issues {
		if i.Code == "recovery.rto_estimate_misses_target" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected recovery.rto_estimate_misses_target: %+v", r.Issues)
	}
}

// TestReadiness_KEKReachable: encrypted backup with a working
// KEKResolver → encryption section reports reachable.
func TestReadiness_KEKReachable(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitBackup(t, "db1", now.Add(-1*time.Hour), 1<<30, true, backup.BackupTypeFull, 1)

	r, err := recovery.Readiness(context.Background(), w.sp, "db1", recovery.Options{
		Verifier:    w.verifier,
		Now:         now,
		KEKResolver: goodResolver(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Encryption == nil {
		t.Fatal("Encryption missing")
	}
	if !r.Encryption.Encrypted {
		t.Errorf("Encrypted = false; want true")
	}
	if !r.Encryption.KEKReachable {
		t.Errorf("KEKReachable = false")
	}
}

// TestReadiness_KEKUnreachable: encrypted backup with a failing
// resolver → critical issue.
func TestReadiness_KEKUnreachable(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitBackup(t, "db1", now.Add(-1*time.Hour), 1<<30, true, backup.BackupTypeFull, 1)

	r, err := recovery.Readiness(context.Background(), w.sp, "db1", recovery.Options{
		Verifier:    w.verifier,
		Now:         now,
		KEKResolver: badResolver("nope"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Encryption == nil || r.Encryption.KEKReachable {
		t.Errorf("KEKReachable should be false; got %+v", r.Encryption)
	}
	if r.OverallStatus != recovery.StatusNotReady {
		t.Errorf("Status = %q, want not_ready", r.OverallStatus)
	}
}

// TestReadiness_PlaintextBackup: an unencrypted manifest reports
// Encrypted=false + reachable=true (nothing to fail on).
func TestReadiness_PlaintextBackup(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitBackup(t, "db1", now.Add(-1*time.Hour), 1<<30, false, backup.BackupTypeFull, 1)

	r, err := recovery.Readiness(context.Background(), w.sp, "db1", recovery.Options{
		Verifier:    w.verifier,
		Now:         now,
		KEKResolver: goodResolver(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Encryption == nil || r.Encryption.Encrypted {
		t.Errorf("Encrypted = true; want false: %+v", r.Encryption)
	}
}

// TestReadiness_SkipFlags: SkipVerification / SkipEncryption /
// SkipWAL suppress their sections.
func TestReadiness_SkipFlags(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitBackup(t, "db1", now.Add(-1*time.Hour), 1<<30, false, backup.BackupTypeFull, 1)

	r, err := recovery.Readiness(context.Background(), w.sp, "db1", recovery.Options{
		Verifier:         w.verifier,
		Now:              now,
		SkipVerification: true,
		SkipEncryption:   true,
		SkipWAL:          true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Verification != nil {
		t.Errorf("Verification = %+v, want nil", r.Verification)
	}
	if r.Encryption != nil {
		t.Errorf("Encryption = %+v, want nil", r.Encryption)
	}
	if r.WAL != nil {
		t.Errorf("WAL = %+v, want nil", r.WAL)
	}
}

// TestReadiness_Validation: programmer-error guards.
func TestReadiness_Validation(t *testing.T) {
	w := setupWorld(t)
	if _, err := recovery.Readiness(context.Background(), nil, "db1", recovery.Options{
		Verifier: w.verifier,
	}); err == nil {
		t.Error("nil sp must error")
	}
	if _, err := recovery.Readiness(context.Background(), w.sp, "db1", recovery.Options{}); err == nil {
		t.Error("nil verifier must error")
	}
	if _, err := recovery.Readiness(context.Background(), w.sp, "", recovery.Options{
		Verifier: w.verifier,
	}); err == nil {
		t.Error("empty deployment must error")
	}
}

// TestRenderReadinessMarkdown_HappyPath: every section's heading
// + the verdict appear.
func TestRenderReadinessMarkdown_HappyPath(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitBackup(t, "db1", now.Add(-1*time.Hour), 1<<30, false, backup.BackupTypeFull, 1)

	r, err := recovery.Readiness(context.Background(), w.sp, "db1", recovery.Options{
		Verifier: w.verifier,
		Now:      now,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.URL = w.repoURL // populated by the CLI; tests inject directly
	var sb strings.Builder
	if err := recovery.RenderReadinessMarkdown(&sb, r); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"# pg_hardstorage recovery readiness",
		"## Verdict",
		"## Latest backup",
		"## RPO",
		"## RTO",
		"## Encryption health",
		"## WAL coverage",
		"## Issues",
		"db1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Markdown missing %q in:\n%s", want, out)
		}
	}
}

// TestRenderReadinessMarkdown_Nil_Errors: rendering nil errors.
func TestRenderReadinessMarkdown_Nil_Errors(t *testing.T) {
	var sb strings.Builder
	if err := recovery.RenderReadinessMarkdown(&sb, nil); err == nil {
		t.Error("expected error for nil report")
	}
}

// ----- Windows tests -----

// TestWindows_Empty: no backups → empty windows + zero coverage.
func TestWindows_Empty(t *testing.T) {
	w := setupWorld(t)
	r, err := recovery.Windows(context.Background(), w.sp, "db1", recovery.WindowsOptions{
		Verifier: w.verifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Coverage.WindowCount != 0 {
		t.Errorf("WindowCount = %d, want 0", r.Coverage.WindowCount)
	}
	if len(r.Windows) != 0 {
		t.Errorf("Windows = %d, want 0", len(r.Windows))
	}
}

// TestWindows_NewestFirst: the windows are sorted newest-first.
func TestWindows_NewestFirst(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitBackup(t, "db1", now.Add(-3*24*time.Hour), 1<<30, false, backup.BackupTypeFull, 1)
	w.commitBackup(t, "db1", now.Add(-1*24*time.Hour), 1<<30, false, backup.BackupTypeFull, 1)
	w.commitBackup(t, "db1", now.Add(-2*24*time.Hour), 1<<30, false, backup.BackupTypeFull, 1)

	r, err := recovery.Windows(context.Background(), w.sp, "db1", recovery.WindowsOptions{
		Verifier: w.verifier,
		Now:      now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Coverage.WindowCount != 3 {
		t.Errorf("WindowCount = %d, want 3", r.Coverage.WindowCount)
	}
	for i := 1; i < len(r.Windows); i++ {
		if r.Windows[i].StoppedAt.After(r.Windows[i-1].StoppedAt) {
			t.Errorf("not newest-first at index %d: %v then %v",
				i, r.Windows[i-1].StoppedAt, r.Windows[i].StoppedAt)
		}
	}
}

// TestWindows_IncludeOlderThan_Filters: cuts off old backups.
func TestWindows_IncludeOlderThan_Filters(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitBackup(t, "db1", now.Add(-100*24*time.Hour), 1<<30, false, backup.BackupTypeFull, 1) // way old
	w.commitBackup(t, "db1", now.Add(-1*24*time.Hour), 1<<30, false, backup.BackupTypeFull, 1)

	r, err := recovery.Windows(context.Background(), w.sp, "db1", recovery.WindowsOptions{
		Verifier:         w.verifier,
		Now:              now,
		IncludeOlderThan: 30 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Coverage.WindowCount != 1 {
		t.Errorf("WindowCount = %d, want 1 (older filtered)", r.Coverage.WindowCount)
	}
}

// TestWindows_ManifestEmbeddedGap_Surfaces: a manifest with a
// wal_gaps record surfaces it in WALGapsFromManifest.
func TestWindows_ManifestEmbeddedGap_Surfaces(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	id := w.commitBackup(t, "db1", now.Add(-1*time.Hour), 1<<30, false, backup.BackupTypeFull, 1)

	// Re-write the manifest to add a wal_gaps record. We do this
	// by reading the manifest, mutating, re-signing, re-committing.
	// The test world's signer matches the verifier so this is OK.
	m, err := w.store.Read(context.Background(), "db1", id, w.verifier)
	if err != nil {
		t.Fatal(err)
	}
	m.WALGaps = []backup.WALGap{{
		SlotName:    "pg_hardstorage_db1",
		Timeline:    1,
		GapStartLSN: "0/3000028",
		GapEndLSN:   "0/30001A0",
		GapBytes:    100,
		DetectedAt:  now.Add(-30 * time.Minute),
	}}
	m.Attestation = nil
	body, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	// Re-sign manually.
	if err := m.Sign(w.signer); err != nil {
		t.Fatal(err)
	}
	body, err = m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	// Overwrite the primary; test fixture only needs the
	// manifest to round-trip the wal_gaps field. We use Put
	// directly to overwrite — operators do this via rotate; here
	// we're test-stubbing the field.
	primary := backup.PrimaryPath("db1", id)
	if _, err := w.sp.Put(context.Background(), primary,
		stringReader(string(body)),
		storage.PutOptions{ContentLength: int64(len(body))},
	); err != nil {
		t.Fatal(err)
	}

	r, err := recovery.Windows(context.Background(), w.sp, "db1", recovery.WindowsOptions{
		Verifier: w.verifier,
		Now:      now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Coverage.WindowsWithGaps != 1 {
		t.Errorf("WindowsWithGaps = %d, want 1", r.Coverage.WindowsWithGaps)
	}
	if r.Coverage.TotalGapBytes != 100 {
		t.Errorf("TotalGapBytes = %d, want 100", r.Coverage.TotalGapBytes)
	}
	if len(r.Windows) != 1 || len(r.Windows[0].WALGapsFromManifest) != 1 {
		t.Errorf("WALGapsFromManifest missing: %+v", r.Windows)
	}
}

// TestRenderWindowsMarkdown_HappyPath
func TestRenderWindowsMarkdown_HappyPath(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitBackup(t, "db1", now.Add(-1*time.Hour), 1<<30, false, backup.BackupTypeFull, 1)

	r, err := recovery.Windows(context.Background(), w.sp, "db1", recovery.WindowsOptions{
		Verifier: w.verifier,
		Now:      now,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.URL = w.repoURL
	var sb strings.Builder
	if err := recovery.RenderWindowsMarkdown(&sb, r); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"# pg_hardstorage recovery windows",
		"## PITR windows",
		"db1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Markdown missing %q in:\n%s", want, out)
		}
	}
}

// TestRenderWindowsMarkdown_NoWindows: empty body still renders.
func TestRenderWindowsMarkdown_NoWindows(t *testing.T) {
	r := &recovery.WindowsReport{
		Schema:      recovery.WindowsSchema,
		Deployment:  "db1",
		URL:         "file:///tmp/x",
		GeneratedAt: time.Now().UTC(),
	}
	var sb strings.Builder
	if err := recovery.RenderWindowsMarkdown(&sb, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "No PITR windows") {
		t.Errorf("expected empty placeholder")
	}
}

// TestWindows_Validation: programmer-error guards.
func TestWindows_Validation(t *testing.T) {
	w := setupWorld(t)
	if _, err := recovery.Windows(context.Background(), nil, "db1", recovery.WindowsOptions{
		Verifier: w.verifier,
	}); err == nil {
		t.Error("nil sp must error")
	}
	if _, err := recovery.Windows(context.Background(), w.sp, "db1", recovery.WindowsOptions{}); err == nil {
		t.Error("nil verifier must error")
	}
	if _, err := recovery.Windows(context.Background(), w.sp, "", recovery.WindowsOptions{
		Verifier: w.verifier,
	}); err == nil {
		t.Error("empty deployment must error")
	}
}

// TestSortIssues_SortedBySeverity: critical < warning < notice.
func TestSortIssues_SortedBySeverity(t *testing.T) {
	in := []recovery.ReadinessIssue{
		{Severity: recovery.SeverityNotice, Code: "n1"},
		{Severity: recovery.SeverityCritical, Code: "c1"},
		{Severity: recovery.SeverityWarning, Code: "w1"},
		{Severity: recovery.SeverityCritical, Code: "c2"},
	}
	recovery.SortIssues(in)
	want := []string{"c1", "c2", "w1", "n1"}
	for i, w := range want {
		if in[i].Code != w {
			t.Errorf("[%d].Code = %q, want %q", i, in[i].Code, w)
		}
	}
}

// TestFormatLSNRange: helper formats correctly.
func TestFormatLSNRange(t *testing.T) {
	cases := []struct {
		start, stop, want string
	}{
		{"0/A", "0/B", "0/A..0/B"},
		{"", "", ""},
		{"0/A", "", "0/A"},
		{"", "0/B", "0/B"},
	}
	for _, c := range cases {
		if got := recovery.FormatLSNRange(c.start, c.stop); got != c.want {
			t.Errorf("FormatLSNRange(%q,%q) = %q, want %q", c.start, c.stop, got, c.want)
		}
	}
}

// stringReader is a tiny io.Reader-from-string for the manifest
// rewrite path. Duplicates internal helpers in other packages but
// kept here so the test file is self-contained.
func stringReader(s string) *strings.Reader { return strings.NewReader(s) }
