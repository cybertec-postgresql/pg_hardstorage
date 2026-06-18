// history.go — DrillHistoryEntry: append-only per-drill record persisted under recovery/drills/.
package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdio "io"
	"sort"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// HistorySchema is the on-disk version tag for DrillHistoryEntry
// bodies.  Stable per the v1 schema commitment.
const HistorySchema = "pg_hardstorage.recovery.drill_history.v1"

// HistoryPrefix is the repo-relative prefix under which drill
// history records live.  One file per drill run; lex order
// matches commit order because each filename embeds the drill's
// generated_at as RFC3339Nano with a unix-second prefix.
const HistoryPrefix = "recovery/drills/"

// DrillHistoryEntry is the slim summary record persisted per
// drill run.  Distinct from DrillReport: the entry is meant for
// fleet-level history scans (list every drill in the last 90
// days), so it strips the verbose Phase / Restore / Verify
// detail down to the headline numbers.  Operators wanting full
// per-drill detail can resolve back to the original DrillReport
// via the audit chain.
type DrillHistoryEntry struct {
	Schema string `json:"schema"`

	// ID uniquely identifies this drill run.  Lexicographically
	// sortable: <unix-seconds>-<deployment>-<short-hash> so a
	// repo's drill history sorts chronologically with one List.
	ID string `json:"id"`

	// Deployment + BackupID identify the manifest that was drilled.
	Deployment string `json:"deployment"`
	BackupID   string `json:"backup_id"`

	// Verdict is the drill's overall outcome ("pass" / "partial"
	// / "fail").
	Verdict DrillVerdict `json:"verdict"`

	// GeneratedAt is when the drill started; StoppedAt is when
	// it returned.  Both UTC.
	GeneratedAt time.Time `json:"generated_at"`
	StoppedAt   time.Time `json:"stopped_at"`
	DurationMS  int64     `json:"duration_ms"`

	// RTOActualSeconds is the wallclock from drill start to
	// successful restore (excluding verify).  Operators trend
	// this across runs to detect regressions.
	RTOActualSeconds int64 `json:"rto_actual_seconds"`

	// RTOEstimateSeconds carries the estimate / target the drill
	// was run against.  Zero when no target was supplied.
	RTOEstimateSeconds int64 `json:"rto_estimate_seconds,omitempty"`

	// Per-phase OK flags.  A renderer reading the history can
	// tell at a glance which phase failed without re-fetching
	// the full report.
	PickOK     bool `json:"pick_ok"`
	PrepareOK  bool `json:"prepare_ok,omitempty"`
	RestoreOK  bool `json:"restore_ok,omitempty"`
	VerifyOK   bool `json:"verify_ok,omitempty"`
	TeardownOK bool `json:"teardown_ok,omitempty"`

	// VerifySkipped + VerifyImage record the verify phase's
	// metadata.  When VerifySkipped is true, VerifyOK reflects
	// whether the skip was acceptable (--allow-skip-verify).
	VerifySkipped bool   `json:"verify_skipped,omitempty"`
	VerifyImage   string `json:"verify_image,omitempty"`

	// IssueCount is the total findings; CriticalCount captures
	// the worst-severity subset.  Renderers typically show
	// IssueCount in the table; CriticalCount in red.
	IssueCount    int `json:"issue_count,omitempty"`
	CriticalCount int `json:"critical_count,omitempty"`

	// Operator records who initiated the drill.  Free-form;
	// empty when the operator didn't supply one (cron-driven
	// scheduled runs typically pass "scheduler:<task-id>").
	Operator string `json:"operator,omitempty"`
}

// SummariseDrillReport projects a DrillReport into the slim
// DrillHistoryEntry shape.  Invoked from Drill() when persistence
// is enabled; idempotent + pure (no I/O).
func SummariseDrillReport(r *DrillReport, operator string) *DrillHistoryEntry {
	if r == nil {
		return nil
	}
	entry := &DrillHistoryEntry{
		Schema:             HistorySchema,
		Deployment:         r.Deployment,
		BackupID:           r.BackupID,
		Verdict:            r.Verdict,
		GeneratedAt:        r.GeneratedAt,
		StoppedAt:          r.StoppedAt,
		DurationMS:         r.DurationMS,
		RTOActualSeconds:   r.RTOActualSeconds,
		RTOEstimateSeconds: r.RTOEstimateSeconds,
		Operator:           operator,
	}
	for _, p := range r.Phases {
		switch p.Name {
		case "pick":
			entry.PickOK = p.OK
		case "prepare":
			entry.PrepareOK = p.OK
		case "restore":
			entry.RestoreOK = p.OK
		case "verify":
			entry.VerifyOK = p.OK
		case "teardown":
			entry.TeardownOK = p.OK
		}
	}
	if r.Verify != nil {
		entry.VerifySkipped = r.Verify.Skipped
		entry.VerifyImage = r.Verify.Image
	}
	for _, iss := range r.Issues {
		entry.IssueCount++
		if iss.Severity == SeverityCritical {
			entry.CriticalCount++
		}
	}
	entry.ID = entry.computeID()
	return entry
}

// computeID returns the lex-sortable ID for this entry.
// Format: <020d-unix-seconds>-<deployment>-<short-hash>
// — the unix-seconds left-pad keeps lex order = chronological
// order indefinitely; the short hash disambiguates concurrent
// drills against the same deployment in the same second.
func (e *DrillHistoryEntry) computeID() string {
	secs := e.GeneratedAt.UTC().Unix()
	dep := sanitizeDeployment(e.Deployment)
	hash := shortHash(e.GeneratedAt, e.BackupID)
	return fmt.Sprintf("%020d-%s-%s", secs, dep, hash)
}

// sanitizeDeployment ensures the deployment name doesn't contain
// path separators or other characters that would break the repo
// key layout.  Replaces non-alphanumeric with '_'.
func sanitizeDeployment(d string) string {
	out := make([]byte, 0, len(d))
	for i := 0; i < len(d); i++ {
		c := d[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "unknown"
	}
	return string(out)
}

// shortHash returns a deterministic 8-char hash from (generated_at,
// backup_id).  Used for the disambiguation suffix on the entry ID.
// FNV-1a is fast enough for this and avoids dragging in crypto
// dependencies for what's essentially a uniqueness tag.
func shortHash(t time.Time, backupID string) string {
	h := fnv1aHash(t.Format(time.RFC3339Nano) + "|" + backupID)
	return fmt.Sprintf("%08x", h)
}

// fnv1aHash is the canonical 32-bit FNV-1a, inlined to avoid
// importing hash/fnv just for the disambiguation tag.
func fnv1aHash(s string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	hash := uint32(offset32)
	for i := 0; i < len(s); i++ {
		hash ^= uint32(s[i])
		hash *= prime32
	}
	return hash
}

// HistoryStore reads + writes drill history records against any
// StoragePlugin.  All operations are atomic at the per-entry
// level: each entry is one write; no read-modify-write.
type HistoryStore struct {
	sp storage.StoragePlugin
}

// NewHistoryStore wraps sp.
func NewHistoryStore(sp storage.StoragePlugin) *HistoryStore {
	if sp == nil {
		panic("recovery: NewHistoryStore requires a non-nil StoragePlugin")
	}
	return &HistoryStore{sp: sp}
}

// historyKeyFor returns the on-disk key for a history entry.
// Layout: recovery/drills/<id>.json
func historyKeyFor(id string) string {
	return HistoryPrefix + id + ".json"
}

// Append persists one drill history entry.  Idempotent: a re-run
// with the same ID is a no-op (the entry's content is
// deterministic from the inputs, so the second write is a
// rewrite of the same bytes).  Uses Put with IfNotExists=false
// so a re-run doesn't fail; that's safe because the ID embeds
// generated_at + backup_id + a short hash — collisions would
// require the same drill to be persisted twice, which is a
// no-op anyway.
func (s *HistoryStore) Append(ctx context.Context, entry *DrillHistoryEntry) error {
	if entry == nil {
		return errors.New("recovery: nil entry")
	}
	if entry.Schema == "" {
		entry.Schema = HistorySchema
	}
	if entry.ID == "" {
		entry.ID = entry.computeID()
	}
	body, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("recovery: marshal history entry: %w", err)
	}
	if _, err := s.sp.Put(ctx, historyKeyFor(entry.ID), bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		return fmt.Errorf("recovery: put history entry: %w", err)
	}
	return nil
}

// HistoryFilter is the (filter) shape passed to List.  Empty /
// zero values mean "no filter".
type HistoryFilter struct {
	// Deployment, when non-empty, restricts to entries for this
	// deployment.
	Deployment string

	// Verdict, when non-empty, restricts to entries with this
	// verdict.
	Verdict DrillVerdict

	// Since / Until bound the GeneratedAt range.  Since is
	// inclusive; Until is exclusive.  Either zero means open-
	// ended on that side.
	Since time.Time
	Until time.Time

	// Limit caps the returned slice.  0 = unbounded.
	Limit int

	// Reverse, when true, returns newest-first.  Default is
	// commit order (oldest-first lex by ID).
	Reverse bool
}

// List walks the recovery/drills/ prefix and returns every entry
// matching the filter.  O(history-length) per call; fleets with
// huge drill volumes can paginate via Since/Until + Limit.
func (s *HistoryStore) List(ctx context.Context, f HistoryFilter) ([]*DrillHistoryEntry, error) {
	keys, err := s.allKeys(ctx)
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	if f.Reverse {
		for i, j := 0, len(keys)-1; i < j; i, j = i+1, j-1 {
			keys[i], keys[j] = keys[j], keys[i]
		}
	}
	var out []*DrillHistoryEntry
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		entry, err := s.get(ctx, k)
		if err != nil {
			// A torn / partial write would surface here as a
			// JSON-decode error.  Skip + continue rather than
			// failing the whole walk; one corrupt history file
			// shouldn't lock out the operator.
			continue
		}
		if !matchesHistoryFilter(entry, f) {
			continue
		}
		out = append(out, entry)
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out, nil
}

// matchesHistoryFilter is the in-memory predicate.  Pure.
func matchesHistoryFilter(e *DrillHistoryEntry, f HistoryFilter) bool {
	if f.Deployment != "" && e.Deployment != f.Deployment {
		return false
	}
	if f.Verdict != "" && e.Verdict != f.Verdict {
		return false
	}
	if !f.Since.IsZero() && e.GeneratedAt.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !e.GeneratedAt.Before(f.Until) {
		return false
	}
	return true
}

func (s *HistoryStore) allKeys(ctx context.Context) ([]string, error) {
	var keys []string
	for info, err := range s.sp.List(ctx, HistoryPrefix) {
		if err != nil {
			return nil, fmt.Errorf("recovery: list history: %w", err)
		}
		if !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		keys = append(keys, info.Key)
	}
	return keys, nil
}

func (s *HistoryStore) get(ctx context.Context, key string) (*DrillHistoryEntry, error) {
	rc, err := s.sp.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("recovery: get history %q: %w", key, err)
	}
	defer rc.Close()
	body, err := stdio.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("recovery: read history %q: %w", key, err)
	}
	var entry DrillHistoryEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		return nil, fmt.Errorf("recovery: decode history %q: %w", key, err)
	}
	return &entry, nil
}

// HistorySummary is the aggregate-view body computed from a
// slice of entries.  Surfaces the rollup operators want at a
// glance: total runs, verdict distribution, RTO percentiles,
// trend direction.
type HistorySummary struct {
	Schema       string    `json:"schema"`
	Total        int       `json:"total"`
	OldestAt     time.Time `json:"oldest_at,omitempty"`
	NewestAt     time.Time `json:"newest_at,omitempty"`
	PassCount    int       `json:"pass_count"`
	PartialCount int       `json:"partial_count,omitempty"`
	FailCount    int       `json:"fail_count,omitempty"`

	// PassPercent is PassCount / Total × 100.  Rounded to 2
	// decimals.  Zero when Total == 0.
	PassPercent float64 `json:"pass_percent"`

	// RTOMinSeconds / RTOMaxSeconds / RTOMedianSeconds /
	// RTOMeanSeconds capture the RTO actual distribution across
	// runs.  All zero when Total == 0 OR when no runs recorded
	// a non-zero RTO actual (every drill failed before restore
	// completed).
	RTOMinSeconds    int64 `json:"rto_min_seconds,omitempty"`
	RTOMaxSeconds    int64 `json:"rto_max_seconds,omitempty"`
	RTOMedianSeconds int64 `json:"rto_median_seconds,omitempty"`
	RTOMeanSeconds   int64 `json:"rto_mean_seconds,omitempty"`

	// LatestVerdict is the verdict of the most-recent run.
	// Useful for "is the deployment recovery-ready right now?"
	// dashboard panels.
	LatestVerdict DrillVerdict `json:"latest_verdict,omitempty"`
	LatestRTO     int64        `json:"latest_rto_seconds,omitempty"`
	LatestAt      time.Time    `json:"latest_at,omitempty"`

	// VerdictTrend captures the recent trend.  Compares the
	// verdict-distribution of the LATEST_TREND_TAIL most-recent
	// runs to the rest.  "improving" / "stable" / "regressing".
	// Empty when Total < LATEST_TREND_TAIL × 2 (insufficient
	// data for a meaningful trend).
	VerdictTrend string `json:"verdict_trend,omitempty"`
}

// HistorySummarySchema is the on-disk version tag for
// HistorySummary bodies.
const HistorySummarySchema = "pg_hardstorage.recovery.drill_history_summary.v1"

// LATEST_TREND_TAIL is the size of the recent slice we compare
// against the rest of the history for the trend signal.  5 is
// small enough to react to a regression quickly + large enough
// that single-run noise doesn't flip the trend.
const LATEST_TREND_TAIL = 5

// Summarize computes the HistorySummary from a slice of entries.
// The slice must be in time order (oldest first); List returns
// it that way by default.
func Summarize(entries []*DrillHistoryEntry) *HistorySummary {
	out := &HistorySummary{Schema: HistorySummarySchema, Total: len(entries)}
	if len(entries) == 0 {
		return out
	}
	out.OldestAt = entries[0].GeneratedAt
	out.NewestAt = entries[len(entries)-1].GeneratedAt
	rtoSamples := make([]int64, 0, len(entries))
	for _, e := range entries {
		switch e.Verdict {
		case DrillVerdictPass:
			out.PassCount++
		case DrillVerdictPartial:
			out.PartialCount++
		case DrillVerdictFail:
			out.FailCount++
		}
		if e.RTOActualSeconds > 0 {
			rtoSamples = append(rtoSamples, e.RTOActualSeconds)
		}
	}
	out.PassPercent = roundPercent(float64(out.PassCount) * 100.0 / float64(out.Total))

	if len(rtoSamples) > 0 {
		sort.Slice(rtoSamples, func(i, j int) bool { return rtoSamples[i] < rtoSamples[j] })
		out.RTOMinSeconds = rtoSamples[0]
		out.RTOMaxSeconds = rtoSamples[len(rtoSamples)-1]
		out.RTOMedianSeconds = rtoSamples[len(rtoSamples)/2]
		var sum int64
		for _, v := range rtoSamples {
			sum += v
		}
		out.RTOMeanSeconds = sum / int64(len(rtoSamples))
	}

	latest := entries[len(entries)-1]
	out.LatestVerdict = latest.Verdict
	out.LatestRTO = latest.RTOActualSeconds
	out.LatestAt = latest.GeneratedAt

	out.VerdictTrend = computeTrend(entries)
	return out
}

// computeTrend compares the LATEST_TREND_TAIL most-recent runs
// to the rest.  Improvement = pass rate of tail > pass rate of
// rest by ≥ 10 percentage points.  Regression = the inverse.
// Stable otherwise.  Empty string when insufficient data.
func computeTrend(entries []*DrillHistoryEntry) string {
	if len(entries) < LATEST_TREND_TAIL*2 {
		return ""
	}
	tailStart := len(entries) - LATEST_TREND_TAIL
	tail := entries[tailStart:]
	rest := entries[:tailStart]
	tailPass := passRate(tail)
	restPass := passRate(rest)
	delta := tailPass - restPass
	switch {
	case delta >= 10:
		return "improving"
	case delta <= -10:
		return "regressing"
	default:
		return "stable"
	}
}

func passRate(entries []*DrillHistoryEntry) float64 {
	if len(entries) == 0 {
		return 0
	}
	pass := 0
	for _, e := range entries {
		if e.Verdict == DrillVerdictPass {
			pass++
		}
	}
	return float64(pass) * 100.0 / float64(len(entries))
}

// roundPercent rounds a float to 2 decimals.  Pure helper.
func roundPercent(p float64) float64 {
	return float64(int64(p*100+0.5)) / 100.0
}
