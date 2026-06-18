package recovery_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/recovery"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// historyWorld provides a bare storage backend (no manifest plumbing
// needed for direct history-store tests).
type historyWorld struct {
	sp      storage.StoragePlugin
	repoURL string
}

func setupHistoryWorld(t *testing.T) *historyWorld {
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
	return &historyWorld{sp: sp, repoURL: repoURL}
}

// mkEntry constructs a synthetic DrillHistoryEntry with reasonable
// defaults.  Tests override fields they care about.
func mkEntry(deployment string, at time.Time, verdict recovery.DrillVerdict, rtoSec int64) *recovery.DrillHistoryEntry {
	e := &recovery.DrillHistoryEntry{
		Deployment:       deployment,
		BackupID:         deployment + ".full." + at.Format("20060102T150405Z"),
		Verdict:          verdict,
		GeneratedAt:      at,
		StoppedAt:        at.Add(30 * time.Second),
		DurationMS:       30000,
		RTOActualSeconds: rtoSec,
		PickOK:           true,
		PrepareOK:        true,
		RestoreOK:        verdict != recovery.DrillVerdictFail,
		VerifyOK:         verdict == recovery.DrillVerdictPass,
		TeardownOK:       true,
	}
	return e
}

// TestHistory_AppendAndList: a single Append round-trips through
// List.
func TestHistory_AppendAndList(t *testing.T) {
	w := setupHistoryWorld(t)
	store := recovery.NewHistoryStore(w.sp)
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	entry := mkEntry("db1", at, recovery.DrillVerdictPass, 47)

	if err := store.Append(context.Background(), entry); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if entry.ID == "" {
		t.Errorf("Append did not assign ID")
	}

	list, err := store.List(context.Background(), recovery.HistoryFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List = %d entries, want 1", len(list))
	}
	got := list[0]
	if got.Deployment != "db1" || got.Verdict != recovery.DrillVerdictPass {
		t.Errorf("got = %+v", got)
	}
	if got.RTOActualSeconds != 47 {
		t.Errorf("RTOActualSeconds = %d, want 47", got.RTOActualSeconds)
	}
}

// TestHistory_NilEntry: Append refuses nil.
func TestHistory_NilEntry(t *testing.T) {
	w := setupHistoryWorld(t)
	store := recovery.NewHistoryStore(w.sp)
	if err := store.Append(context.Background(), nil); err == nil {
		t.Error("Append(nil) must error")
	}
}

// TestHistory_DeploymentFilter: filter by deployment.
func TestHistory_DeploymentFilter(t *testing.T) {
	w := setupHistoryWorld(t)
	store := recovery.NewHistoryStore(w.sp)
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := store.Append(context.Background(), mkEntry("db1", at, recovery.DrillVerdictPass, 47)); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), mkEntry("db2", at.Add(time.Minute), recovery.DrillVerdictPass, 50)); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), mkEntry("db1", at.Add(2*time.Minute), recovery.DrillVerdictFail, 0)); err != nil {
		t.Fatal(err)
	}

	list, err := store.List(context.Background(), recovery.HistoryFilter{Deployment: "db1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("filtered = %d, want 2", len(list))
	}
	for _, e := range list {
		if e.Deployment != "db1" {
			t.Errorf("filter leaked: %+v", e)
		}
	}
}

// TestHistory_VerdictFilter
func TestHistory_VerdictFilter(t *testing.T) {
	w := setupHistoryWorld(t)
	store := recovery.NewHistoryStore(w.sp)
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i, v := range []recovery.DrillVerdict{
		recovery.DrillVerdictPass,
		recovery.DrillVerdictFail,
		recovery.DrillVerdictPass,
		recovery.DrillVerdictPartial,
	} {
		if err := store.Append(context.Background(), mkEntry("db1",
			at.Add(time.Duration(i)*time.Minute), v, 47)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := store.List(context.Background(), recovery.HistoryFilter{
		Verdict: recovery.DrillVerdictPass,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("pass-only = %d, want 2", len(list))
	}
}

// TestHistory_TimeRangeFilter: Since/Until bounds.
func TestHistory_TimeRangeFilter(t *testing.T) {
	w := setupHistoryWorld(t)
	store := recovery.NewHistoryStore(w.sp)
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := store.Append(context.Background(), mkEntry("db1",
			base.Add(time.Duration(i)*24*time.Hour),
			recovery.DrillVerdictPass, 47)); err != nil {
			t.Fatal(err)
		}
	}
	// Since the second day, until the fourth (exclusive).
	list, err := store.List(context.Background(), recovery.HistoryFilter{
		Since: base.Add(1 * 24 * time.Hour),
		Until: base.Add(4 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Errorf("range-filtered = %d, want 3", len(list))
	}
}

// TestHistory_Limit
func TestHistory_Limit(t *testing.T) {
	w := setupHistoryWorld(t)
	store := recovery.NewHistoryStore(w.sp)
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		if err := store.Append(context.Background(), mkEntry("db1",
			at.Add(time.Duration(i)*time.Minute),
			recovery.DrillVerdictPass, 47)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := store.List(context.Background(), recovery.HistoryFilter{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Errorf("limit = %d, want 3", len(list))
	}
}

// TestHistory_Reverse: newest-first ordering.
func TestHistory_Reverse(t *testing.T) {
	w := setupHistoryWorld(t)
	store := recovery.NewHistoryStore(w.sp)
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		if err := store.Append(context.Background(), mkEntry("db1",
			base.Add(time.Duration(i)*time.Hour),
			recovery.DrillVerdictPass, 47)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := store.List(context.Background(), recovery.HistoryFilter{Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("reverse = %d, want 4", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i].GeneratedAt.After(list[i-1].GeneratedAt) {
			t.Errorf("not reverse-sorted at %d: %v vs %v",
				i, list[i-1].GeneratedAt, list[i].GeneratedAt)
		}
	}
}

// TestSummariseDrillReport_NilSafe: handles nil cleanly.
func TestSummariseDrillReport_NilSafe(t *testing.T) {
	if recovery.SummariseDrillReport(nil, "") != nil {
		t.Error("nil report should map to nil entry")
	}
}

// TestSummariseDrillReport_PerPhasePopulated: phase OK flags
// reflect the report's phases.
func TestSummariseDrillReport_PerPhasePopulated(t *testing.T) {
	r := &recovery.DrillReport{
		Schema:      recovery.DrillSchema,
		Deployment:  "db1",
		BackupID:    "db1.full.x",
		Verdict:     recovery.DrillVerdictPass,
		GeneratedAt: time.Now().UTC(),
		Phases: []recovery.DrillPhase{
			{Name: "pick", OK: true},
			{Name: "prepare", OK: true},
			{Name: "restore", OK: true},
			{Name: "verify", OK: true},
			{Name: "teardown", OK: false}, // teardown failed
		},
	}
	entry := recovery.SummariseDrillReport(r, "alice@example.com")
	if !entry.PickOK || !entry.PrepareOK || !entry.RestoreOK || !entry.VerifyOK {
		t.Errorf("phase flags off: %+v", entry)
	}
	if entry.TeardownOK {
		t.Errorf("TeardownOK = true; want false")
	}
	if entry.Operator != "alice@example.com" {
		t.Errorf("Operator = %q", entry.Operator)
	}
}

// TestSummariseDrillReport_IssueCounts: critical issues land in
// CriticalCount.
func TestSummariseDrillReport_IssueCounts(t *testing.T) {
	r := &recovery.DrillReport{
		Schema:      recovery.DrillSchema,
		Deployment:  "db1",
		BackupID:    "db1.full.x",
		Verdict:     recovery.DrillVerdictFail,
		GeneratedAt: time.Now().UTC(),
		Issues: []recovery.ReadinessIssue{
			{Severity: recovery.SeverityCritical, Code: "x"},
			{Severity: recovery.SeverityCritical, Code: "y"},
			{Severity: recovery.SeverityWarning, Code: "z"},
			{Severity: recovery.SeverityNotice, Code: "n"},
		},
	}
	entry := recovery.SummariseDrillReport(r, "")
	if entry.IssueCount != 4 {
		t.Errorf("IssueCount = %d, want 4", entry.IssueCount)
	}
	if entry.CriticalCount != 2 {
		t.Errorf("CriticalCount = %d, want 2", entry.CriticalCount)
	}
}

// TestSummarize_Empty: zero entries → zeroed summary.
func TestSummarize_Empty(t *testing.T) {
	s := recovery.Summarize(nil)
	if s == nil || s.Total != 0 {
		t.Errorf("nil → %+v", s)
	}
	if s.Schema == "" {
		t.Error("Schema not set")
	}
}

// TestSummarize_PassRate
func TestSummarize_PassRate(t *testing.T) {
	at := time.Now().UTC()
	entries := []*recovery.DrillHistoryEntry{
		mkEntry("db1", at, recovery.DrillVerdictPass, 47),
		mkEntry("db1", at.Add(time.Minute), recovery.DrillVerdictPass, 50),
		mkEntry("db1", at.Add(2*time.Minute), recovery.DrillVerdictFail, 0),
		mkEntry("db1", at.Add(3*time.Minute), recovery.DrillVerdictPartial, 49),
	}
	s := recovery.Summarize(entries)
	if s.Total != 4 || s.PassCount != 2 || s.FailCount != 1 || s.PartialCount != 1 {
		t.Errorf("counts off: %+v", s)
	}
	if s.PassPercent != 50.00 {
		t.Errorf("PassPercent = %v, want 50.00", s.PassPercent)
	}
}

// TestSummarize_RTODistribution
func TestSummarize_RTODistribution(t *testing.T) {
	at := time.Now().UTC()
	entries := []*recovery.DrillHistoryEntry{
		mkEntry("db1", at, recovery.DrillVerdictPass, 30),
		mkEntry("db1", at.Add(1*time.Minute), recovery.DrillVerdictPass, 40),
		mkEntry("db1", at.Add(2*time.Minute), recovery.DrillVerdictPass, 50),
		mkEntry("db1", at.Add(3*time.Minute), recovery.DrillVerdictPass, 60),
	}
	s := recovery.Summarize(entries)
	if s.RTOMinSeconds != 30 || s.RTOMaxSeconds != 60 {
		t.Errorf("min/max off: %+v", s)
	}
	if s.RTOMedianSeconds != 50 {
		t.Errorf("median = %d, want 50 (sorted [30,40,50,60][2])", s.RTOMedianSeconds)
	}
	if s.RTOMeanSeconds != 45 {
		t.Errorf("mean = %d, want 45", s.RTOMeanSeconds)
	}
}

// TestSummarize_TrendImproving: 5 fails followed by 5 passes →
// improving.
func TestSummarize_TrendImproving(t *testing.T) {
	at := time.Now().UTC()
	var entries []*recovery.DrillHistoryEntry
	for i := 0; i < 5; i++ {
		entries = append(entries, mkEntry("db1",
			at.Add(time.Duration(i)*time.Minute),
			recovery.DrillVerdictFail, 0))
	}
	for i := 0; i < 5; i++ {
		entries = append(entries, mkEntry("db1",
			at.Add(time.Duration(5+i)*time.Minute),
			recovery.DrillVerdictPass, 50))
	}
	s := recovery.Summarize(entries)
	if s.VerdictTrend != "improving" {
		t.Errorf("VerdictTrend = %q, want improving", s.VerdictTrend)
	}
}

// TestSummarize_TrendRegressing
func TestSummarize_TrendRegressing(t *testing.T) {
	at := time.Now().UTC()
	var entries []*recovery.DrillHistoryEntry
	for i := 0; i < 5; i++ {
		entries = append(entries, mkEntry("db1",
			at.Add(time.Duration(i)*time.Minute),
			recovery.DrillVerdictPass, 50))
	}
	for i := 0; i < 5; i++ {
		entries = append(entries, mkEntry("db1",
			at.Add(time.Duration(5+i)*time.Minute),
			recovery.DrillVerdictFail, 0))
	}
	s := recovery.Summarize(entries)
	if s.VerdictTrend != "regressing" {
		t.Errorf("VerdictTrend = %q, want regressing", s.VerdictTrend)
	}
}

// TestSummarize_TrendStable
func TestSummarize_TrendStable(t *testing.T) {
	at := time.Now().UTC()
	var entries []*recovery.DrillHistoryEntry
	for i := 0; i < 10; i++ {
		entries = append(entries, mkEntry("db1",
			at.Add(time.Duration(i)*time.Minute),
			recovery.DrillVerdictPass, 50))
	}
	s := recovery.Summarize(entries)
	if s.VerdictTrend != "stable" {
		t.Errorf("VerdictTrend = %q, want stable", s.VerdictTrend)
	}
}

// TestSummarize_TrendInsufficientData: < LATEST_TREND_TAIL × 2
// returns empty.
func TestSummarize_TrendInsufficientData(t *testing.T) {
	at := time.Now().UTC()
	var entries []*recovery.DrillHistoryEntry
	for i := 0; i < 3; i++ {
		entries = append(entries, mkEntry("db1",
			at.Add(time.Duration(i)*time.Minute),
			recovery.DrillVerdictPass, 50))
	}
	s := recovery.Summarize(entries)
	if s.VerdictTrend != "" {
		t.Errorf("VerdictTrend = %q, want empty (sparse data)", s.VerdictTrend)
	}
}

// TestHistory_DrillAutoPersists: Drill() with default options
// auto-persists a slim entry.
func TestHistory_DrillAutoPersists(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackup(t, "db1", time.Now().UTC().Add(-1*time.Hour), 100)

	r, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptions{
		Verifier:           w.verifier,
		SkipVerifyEntirely: true,
	})
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	_ = r

	// Read the history.
	store := recovery.NewHistoryStore(w.sp)
	list, err := store.List(context.Background(), recovery.HistoryFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("history = %d, want 1 (auto-persisted)", len(list))
	}
}

// TestHistory_DrillSkipHistory: SkipHistory=true suppresses the
// auto-persist.
func TestHistory_DrillSkipHistory(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackup(t, "db1", time.Now().UTC().Add(-1*time.Hour), 100)

	if _, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptions{
		Verifier:           w.verifier,
		SkipVerifyEntirely: true,
		SkipHistory:        true,
	}); err != nil {
		t.Fatalf("Drill: %v", err)
	}

	store := recovery.NewHistoryStore(w.sp)
	list, err := store.List(context.Background(), recovery.HistoryFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("history = %d, want 0 (SkipHistory)", len(list))
	}
}

// TestHistory_DrillRecordsOperator: Operator field flows from
// DrillOptions into the persisted entry.
func TestHistory_DrillRecordsOperator(t *testing.T) {
	w := setupDrillWorld(t)
	w.commitDrillBackup(t, "db1", time.Now().UTC().Add(-1*time.Hour), 100)

	if _, err := recovery.Drill(context.Background(), w.repoURL, "db1", recovery.DrillOptions{
		Verifier:           w.verifier,
		SkipVerifyEntirely: true,
		Operator:           "scheduler:weekly-drill",
	}); err != nil {
		t.Fatalf("Drill: %v", err)
	}
	store := recovery.NewHistoryStore(w.sp)
	list, _ := store.List(context.Background(), recovery.HistoryFilter{})
	if len(list) != 1 || list[0].Operator != "scheduler:weekly-drill" {
		t.Errorf("Operator not recorded: %+v", list)
	}
}

// TestHistory_IDIsLexSortable: entries written with newer
// timestamps sort lex-after older ones.
func TestHistory_IDIsLexSortable(t *testing.T) {
	w := setupHistoryWorld(t)
	store := recovery.NewHistoryStore(w.sp)
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := store.Append(context.Background(), mkEntry("db1",
			base.Add(time.Duration(i)*time.Hour),
			recovery.DrillVerdictPass, 47)); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := store.List(context.Background(), recovery.HistoryFilter{})
	for i := 1; i < len(list); i++ {
		if list[i].ID <= list[i-1].ID {
			t.Errorf("IDs not lex-sorted at %d: %q vs %q",
				i, list[i-1].ID, list[i].ID)
		}
	}
}

// TestHistory_IDDeterministic: SummariseDrillReport produces the
// same ID for the same input — round-tripping a report doesn't
// duplicate history entries.
func TestHistory_IDDeterministic(t *testing.T) {
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	r := &recovery.DrillReport{
		Schema:      recovery.DrillSchema,
		Deployment:  "db1",
		BackupID:    "db1.full.X",
		Verdict:     recovery.DrillVerdictPass,
		GeneratedAt: at,
	}
	a := recovery.SummariseDrillReport(r, "")
	b := recovery.SummariseDrillReport(r, "")
	if a.ID == "" || a.ID != b.ID {
		t.Errorf("IDs mismatched: %q vs %q", a.ID, b.ID)
	}
}

// TestHistory_DeploymentSanitization: special chars in deployment
// don't break the entry ID layout.
func TestHistory_DeploymentSanitization(t *testing.T) {
	at := time.Now().UTC()
	r := &recovery.DrillReport{
		Schema:      recovery.DrillSchema,
		Deployment:  "weird/dep@$%name", // pathological
		BackupID:    "x.full.Y",
		Verdict:     recovery.DrillVerdictPass,
		GeneratedAt: at,
	}
	entry := recovery.SummariseDrillReport(r, "")
	if entry.ID == "" {
		t.Errorf("ID empty")
	}
	// Should not contain the original special chars.
	for _, c := range []byte{'/', '@', '$', '%'} {
		if want := byte(c); contains(entry.ID, want) {
			t.Errorf("ID contains forbidden char %q: %q", c, entry.ID)
		}
	}
}

func contains(s string, c byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return true
		}
	}
	return false
}
