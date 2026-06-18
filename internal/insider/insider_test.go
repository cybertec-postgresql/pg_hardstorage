package insider_test

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/insider"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// fixture wraps a fresh repo + audit store + detector.
type fixture struct {
	sp         storage.StoragePlugin
	auditStore *audit.Store
	detector   *insider.Detector
	scanStore  *insider.ScanStore
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
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
	auditStore := audit.NewStore(sp)
	return &fixture{
		sp:         sp,
		auditStore: auditStore,
		detector:   insider.NewDetector(auditStore),
		scanStore:  insider.NewScanStore(sp),
	}
}

// plant is a tiny helper that appends an audit event with explicit
// timestamp/actor/tenant/action.  Returns the event's stored copy.
func (f *fixture) plant(t *testing.T, at time.Time, actor, tenant, action string) *audit.Event {
	t.Helper()
	ev := &audit.Event{
		Action:    action,
		Actor:     actor,
		Tenant:    tenant,
		Subject:   audit.Subject{Tenant: tenant},
		Timestamp: at,
	}
	if err := f.auditStore.Append(context.Background(), ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
	return ev
}

// findingsByType extracts all findings of the given type from the
// scan.  Helps tests assert per-rule outcomes.
func findingsByType(scan *insider.Scan, t insider.FindingType) []insider.Finding {
	var out []insider.Finding
	for _, f := range scan.Findings {
		if f.Type == t {
			out = append(out, f)
		}
	}
	return out
}

// ----- options validation -----

func TestRun_InvalidWindow(t *testing.T) {
	f := newFixture(t)
	_, err := f.detector.Run(context.Background(), insider.Options{
		BaselineWindow: -1, TargetWindow: time.Hour,
		Now: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	})
	if !errors.Is(err, insider.ErrInvalidWindow) {
		t.Errorf("err = %v, want ErrInvalidWindow", err)
	}
}

// ----- detection rules -----

func TestRun_DetectsNovelPrincipal(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Baseline: alice does some routine reads.
	f.plant(t, now.Add(-15*24*time.Hour), "alice@acme", "default", "backup.read")
	f.plant(t, now.Add(-10*24*time.Hour), "alice@acme", "default", "backup.read")
	// Target: bob shows up out of nowhere.
	f.plant(t, now.Add(-1*time.Hour), "bob@acme", "default", "backup.read")

	scan, err := f.detector.Run(context.Background(), insider.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	novel := findingsByType(scan, insider.FindingNovelPrincipal)
	if len(novel) != 1 {
		t.Fatalf("len(novel) = %d, want 1", len(novel))
	}
	if novel[0].Actor != "bob@acme" {
		t.Errorf("Actor = %q", novel[0].Actor)
	}
	if novel[0].Severity != insider.SeverityWarning {
		t.Errorf("Severity = %s", novel[0].Severity)
	}
}

func TestRun_DetectsFirstDestructive(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Baseline: alice does only reads.
	f.plant(t, now.Add(-20*24*time.Hour), "alice@acme", "default", "backup.read")
	f.plant(t, now.Add(-5*24*time.Hour), "alice@acme", "default", "backup.read")
	// Target: alice runs kms.shred for the first time.
	f.plant(t, now.Add(-30*time.Minute), "alice@acme", "default", "kms.shred")

	scan, err := f.detector.Run(context.Background(), insider.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	fd := findingsByType(scan, insider.FindingFirstDestructive)
	if len(fd) != 1 {
		t.Fatalf("len(fd) = %d, want 1", len(fd))
	}
	if fd[0].Action != "kms.shred" {
		t.Errorf("Action = %q", fd[0].Action)
	}
	if fd[0].Severity != insider.SeverityCritical {
		t.Errorf("Severity = %s, want critical", fd[0].Severity)
	}
}

func TestRun_DetectsOffHoursDestructive(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Baseline: alice does kms.shred only at hour 14 UTC.
	hr14 := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC)
	f.plant(t, hr14, "alice@acme", "default", "kms.shred")
	f.plant(t, hr14.Add(-7*24*time.Hour), "alice@acme", "default", "kms.shred")
	// Target: alice does kms.shred at hour 3 UTC (off-hours).
	target := time.Date(2026, 5, 1, 3, 0, 0, 0, time.UTC)
	f.plant(t, target, "alice@acme", "default", "kms.shred")

	scan, err := f.detector.Run(context.Background(), insider.Options{
		Now: now, TargetWindow: 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	off := findingsByType(scan, insider.FindingOffHoursDestructive)
	if len(off) != 1 {
		t.Fatalf("len(off) = %d, want 1", len(off))
	}
	if off[0].HourOfDay != 3 {
		t.Errorf("HourOfDay = %d", off[0].HourOfDay)
	}
}

func TestRun_DetectsVolumeSpike(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Baseline 30d: 12 backup.read events spread evenly.
	for i := 0; i < 12; i++ {
		f.plant(t, now.Add(-time.Duration(i+1)*48*time.Hour),
			"alice@acme", "default", "backup.read")
	}
	// Target 24h: 30 backup.read events in one day → > 5x baseline rate.
	for i := 0; i < 30; i++ {
		f.plant(t, now.Add(-time.Duration(i+1)*time.Minute),
			"alice@acme", "default", "backup.read")
	}

	scan, err := f.detector.Run(context.Background(), insider.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	spikes := findingsByType(scan, insider.FindingVolumeSpike)
	if len(spikes) != 1 {
		t.Fatalf("len(spikes) = %d, want 1\nfindings: %+v", len(spikes), scan.Findings)
	}
	if spikes[0].TargetCount != 30 {
		t.Errorf("TargetCount = %d, want 30", spikes[0].TargetCount)
	}
	if spikes[0].BaselineRate <= 0 {
		t.Errorf("BaselineRate = %f", spikes[0].BaselineRate)
	}
}

func TestRun_DetectsCrossTenantNovel(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Baseline: alice in tenant-a only.
	f.plant(t, now.Add(-15*24*time.Hour), "alice@acme", "tenant-a", "backup.read")
	f.plant(t, now.Add(-7*24*time.Hour), "alice@acme", "tenant-a", "backup.read")
	// Target: alice touches tenant-b for the first time.
	f.plant(t, now.Add(-30*time.Minute), "alice@acme", "tenant-b", "backup.read")

	scan, err := f.detector.Run(context.Background(), insider.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	xt := findingsByType(scan, insider.FindingCrossTenantNovel)
	if len(xt) != 1 {
		t.Fatalf("len(xt) = %d, want 1", len(xt))
	}
	if xt[0].Tenant != "tenant-b" {
		t.Errorf("Tenant = %q", xt[0].Tenant)
	}
}

func TestRun_DetectsPostJITDestructive(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Baseline: alice has done both jit.issue + kms.shred before.
	f.plant(t, now.Add(-15*24*time.Hour), "alice@acme", "default", "jit.issue")
	f.plant(t, now.Add(-15*24*time.Hour), "alice@acme", "default", "kms.shred")
	// Target: jit.issue at 11:00, kms.shred at 11:30 → within 1h.
	f.plant(t, now.Add(-1*time.Hour), "alice@acme", "default", "jit.issue")
	f.plant(t, now.Add(-30*time.Minute), "alice@acme", "default", "kms.shred")

	scan, err := f.detector.Run(context.Background(), insider.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	pj := findingsByType(scan, insider.FindingPostJITDestructive)
	if len(pj) != 1 {
		t.Fatalf("len(pj) = %d, want 1", len(pj))
	}
	if pj[0].Severity != insider.SeverityNotice {
		t.Errorf("Severity = %s, want notice", pj[0].Severity)
	}
	if len(pj[0].EventIDs) != 2 {
		t.Errorf("EventIDs len = %d, want 2", len(pj[0].EventIDs))
	}
}

func TestRun_PostJITDestructive_OutsideWindow(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Baseline: alice has done both jit.issue + kms.shred before.
	f.plant(t, now.Add(-15*24*time.Hour), "alice@acme", "default", "jit.issue")
	f.plant(t, now.Add(-15*24*time.Hour), "alice@acme", "default", "kms.shred")
	// Target: jit.issue at 09:00, kms.shred at 11:30 → > 1h apart.
	f.plant(t, now.Add(-3*time.Hour), "alice@acme", "default", "jit.issue")
	f.plant(t, now.Add(-30*time.Minute), "alice@acme", "default", "kms.shred")

	scan, err := f.detector.Run(context.Background(), insider.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	pj := findingsByType(scan, insider.FindingPostJITDestructive)
	if len(pj) != 0 {
		t.Errorf("len(pj) = %d, want 0 (jit-shred too far apart)", len(pj))
	}
}

func TestRun_HappyClean(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Same actor, same tenant, same actions, no spikes.
	for i := 0; i < 20; i++ {
		f.plant(t, now.Add(-time.Duration(i+1)*24*time.Hour),
			"alice@acme", "default", "backup.read")
	}
	f.plant(t, now.Add(-1*time.Hour), "alice@acme", "default", "backup.read")

	scan, err := f.detector.Run(context.Background(), insider.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Findings) != 0 {
		t.Errorf("expected no findings, got %d:\n%+v", len(scan.Findings), scan.Findings)
	}
	if scan.HighestSeverity() != "" {
		t.Errorf("HighestSeverity = %s, want empty", scan.HighestSeverity())
	}
}

func TestRun_TenantRestriction(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// alice is normal in tenant-a; bob suddenly appears in tenant-b.
	f.plant(t, now.Add(-15*24*time.Hour), "alice@acme", "tenant-a", "backup.read")
	f.plant(t, now.Add(-15*24*time.Hour), "alice@acme", "tenant-a", "backup.read")
	f.plant(t, now.Add(-1*time.Hour), "alice@acme", "tenant-a", "backup.read")
	f.plant(t, now.Add(-30*time.Minute), "bob@acme", "tenant-b", "backup.read")

	// Scan tenant-a only — bob's appearance in tenant-b is invisible.
	scan, err := f.detector.Run(context.Background(), insider.Options{
		Now: now, Tenant: "tenant-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Findings) != 0 {
		t.Errorf("tenant-scoped scan should ignore tenant-b: %+v", scan.Findings)
	}

	// Scan all tenants — bob is novel.
	scan, err = f.detector.Run(context.Background(), insider.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	novel := findingsByType(scan, insider.FindingNovelPrincipal)
	if len(novel) != 1 || novel[0].Actor != "bob@acme" {
		t.Errorf("expected bob as novel: %+v", novel)
	}
}

func TestRun_HighestSeverity(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// Baseline: alice does backup.read (no destructive).
	for i := 0; i < 12; i++ {
		f.plant(t, now.Add(-time.Duration(i+1)*48*time.Hour),
			"alice@acme", "default", "backup.read")
	}
	// Target: alice runs kms.shred (FirstDestructive → critical).
	f.plant(t, now.Add(-30*time.Minute), "alice@acme", "default", "kms.shred")

	scan, err := f.detector.Run(context.Background(), insider.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if scan.HighestSeverity() != insider.SeverityCritical {
		t.Errorf("HighestSeverity = %s, want critical", scan.HighestSeverity())
	}
}

// ----- store round-trip -----

func TestScanStore_RoundTrip(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	f.plant(t, now.Add(-1*time.Hour), "alice@acme", "default", "backup.read")
	scan, err := f.detector.Run(context.Background(), insider.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.scanStore.Put(context.Background(), scan); err != nil {
		t.Fatal(err)
	}
	got, err := f.scanStore.Get(context.Background(), scan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != scan.ID || got.TargetEvents != scan.TargetEvents {
		t.Errorf("round-trip drift: %+v vs %+v", got, scan)
	}
}

func TestScanStore_GetMissing(t *testing.T) {
	f := newFixture(t)
	_, err := f.scanStore.Get(context.Background(), "ghost")
	if !errors.Is(err, insider.ErrScanNotFound) {
		t.Errorf("err = %v, want ErrScanNotFound", err)
	}
}

func TestScanStore_ListFiltering(t *testing.T) {
	f := newFixture(t)
	// Three scans at three times.  Two clean, one with a finding.
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		ts := now.Add(time.Duration(i) * time.Hour)
		// Plant a destructive event in the third one to produce a finding.
		if i == 2 {
			f.plant(t, ts.Add(-30*time.Minute), "alice@acme", "default", "kms.shred")
		}
		scan, err := f.detector.Run(context.Background(), insider.Options{Now: ts})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.scanStore.Put(context.Background(), scan); err != nil {
			t.Fatal(err)
		}
	}

	all, err := f.scanStore.List(context.Background(), insider.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("len = %d, want 3", len(all))
	}
	// Newest first.
	if !all[0].StartedAt.After(all[1].StartedAt) ||
		!all[1].StartedAt.After(all[2].StartedAt) {
		t.Errorf("not newest-first")
	}

	// HasFindingsOnly should drop the two clean ones.
	scoped, err := f.scanStore.List(context.Background(), insider.ListFilter{
		HasFindingsOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 {
		t.Errorf("HasFindingsOnly len = %d, want 1", len(scoped))
	}
	if len(scoped[0].Findings) == 0 {
		t.Errorf("scan has no findings, but the filter selected it")
	}

	// MinSeverity warning should match the same single scan.
	scoped, err = f.scanStore.List(context.Background(), insider.ListFilter{
		MinSeverity: insider.SeverityWarning,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 {
		t.Errorf("MinSeverity warning len = %d, want 1", len(scoped))
	}
}
