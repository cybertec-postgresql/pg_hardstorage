// Package report carries the schema + renderers for the soak-
// run summary that lands in <report-dir>/{report.md,
// report.json} at the end of every `validate` invocation.
//
// The JSON form is the contract — operators scrape it for
// dashboards, CI compares pass/fail across runs.  The Markdown
// is rendered from the same Report value so the two are
// guaranteed to agree.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Schema string carried in every JSON report.  Bumped on
// incompatible changes; readers refuse anything else.
const Schema = "pg_hardstorage.testkit.report.v1"

// Report is the top-level structure.  Marshals to JSON cleanly;
// WriteMarkdown produces the operator-facing summary.
type Report struct {
	Schema       string        `json:"schema"`
	Project      string        `json:"project"`
	Seed         int64         `json:"seed"`
	StartedAt    time.Time     `json:"started_at"`
	EndedAt      time.Time     `json:"ended_at"`
	Duration     time.Duration `json:"duration_nanos"`
	OverallPass  bool          `json:"overall_pass"`
	FleetSummary FleetSummary  `json:"fleet"`
	Cells        []CellReport  `json:"cells"`
	FaultStats   FaultStats    `json:"fault_stats"`
	Failures     []Failure     `json:"failures,omitempty"`
}

// FleetSummary is the aggregated fleet shape that ran.
type FleetSummary struct {
	TotalCells      int            `json:"total_cells"`
	TotalContainers int            `json:"total_containers"`
	OSDistribution  map[string]int `json:"os_distribution"`
	PGDistribution  map[string]int `json:"pg_distribution"`
}

// CellReport is per-cell outcome.
type CellReport struct {
	Name              string        `json:"name"`
	OS                string        `json:"os"`
	PG                string        `json:"pg"`
	Arch              string        `json:"arch"`
	Role              string        `json:"role"`
	BackupsTaken      int           `json:"backups_taken"`
	BackupsFailed     int           `json:"backups_failed"`
	RestoresAttempted int           `json:"restores_attempted"`
	RestoresFailed    int           `json:"restores_failed"`
	FaultsApplied     int           `json:"faults_applied"`
	IterationsRun     int           `json:"iterations_run"`
	LastIteration     int           `json:"last_iteration"`
	UpFor             time.Duration `json:"up_for_nanos"`
	Pass              bool          `json:"pass"`
	FirstFailureMsg   string        `json:"first_failure_msg,omitempty"`

	// LoadStats is populated when the cell ran a sustained
	// background writer or a continuous WAL stream during the
	// soak.  Empty when neither sidecar was active — the
	// per-cell summary stays compact for runs that don't need
	// load measurement.  See LoadStats for field semantics.
	LoadStats *LoadStats `json:"load_stats,omitempty"`
}

// LoadStats captures per-cell measurements gathered around a
// run's sustained writer + WAL streamer.  All fields are
// optional; a zero value means "we didn't measure that".
//
// Sampling design: PG-side counters (TPS, WAL bytes) are read
// from pg_stat_database / pg_stat_wal at start and end of the
// soak; deltas land here.  Latency comes from pgbench's own
// final report (parsed by the runtime).  WAL stream lag is
// the bytes the streamer was behind pg_current_wal_lsn at
// teardown.
type LoadStats struct {
	// TPSAvg is end-to-end transactions / second over the
	// sustained writer's wall-clock lifetime, including any
	// pauses for backups + faults.  Sourced from pgbench's
	// "tps = X" report line.
	TPSAvg float64 `json:"tps_avg,omitempty"`

	// LatencyP95Ms is the 95th-percentile transaction latency
	// from the sustained writer's pgbench report.  Captures
	// the source-side overhead of running backups + faults
	// concurrently with writes.
	LatencyP95Ms float64 `json:"latency_p95_ms,omitempty"`

	// WALBytesWritten is the delta of pg_stat_wal.wal_bytes
	// between Start and Stop of the writer.  Represents the
	// upstream-of-streamer pressure the WAL transport had to
	// keep up with.
	WALBytesWritten int64 `json:"wal_bytes_written,omitempty"`

	// WALStreamLagBytes is `pg_current_wal_lsn() -
	// max(flush_lsn)` at Stop time, in bytes.  Sourced
	// live-side from pg_stat_replication: zero means
	// "consumer is fully caught up or disconnected"
	// (both the right operator signal — render as "—").
	WALStreamLagBytes int64 `json:"wal_stream_lag_bytes,omitempty"`

	// WALRepoLagBytes is `pg_current_wal_lsn() - max
	// end_lsn across segments committed to the repo`,
	// also in bytes at Stop time.  Complements
	// WALStreamLagBytes: the live-side lag tracks the
	// in-flight TCP send/flush gap, this tracks the
	// "did segments actually durably commit to the
	// backing store?" gap.  Both being near-zero is the
	// healthy signal.  -1 means we couldn't query the
	// repo (agent binary missing, repo unreachable, etc.).
	WALRepoLagBytes int64 `json:"wal_repo_lag_bytes,omitempty"`

	// WALSegmentsCommitted is the count of WAL segments
	// the repo has committed at Stop time.  A count of 0
	// alongside non-zero WALBytesWritten is the smoking
	// gun for "WAL stream sidecar started but never
	// committed any segments" — exactly the kind of
	// silent-failure mode the agent's wal stream
	// machinery is supposed to prevent but that no other
	// metric catches.
	WALSegmentsCommitted int `json:"wal_segments_committed,omitempty"`

	// SustainedWriterRan is true iff the sustained writer
	// actually started (profile asked for it AND the runtime
	// successfully launched it).  Distinguishes "writer
	// configured but failed to start" from "writer never
	// configured" — both leave numeric fields zero, this is
	// the disambiguator.
	SustainedWriterRan bool `json:"sustained_writer_ran,omitempty"`

	// WALStreamRan is the same flag for the wal-stream
	// sidecar.
	WALStreamRan bool `json:"wal_stream_ran,omitempty"`
}

// FaultStats are aggregated injection counts.
type FaultStats struct {
	TotalApplied  int            `json:"total_applied"`
	ByPrefix      map[string]int `json:"by_prefix"`
	RecoveryFails int            `json:"recovery_fails"`
}

// Failure carries one assertion-level failure surfaced during
// the soak.  The reproducer-tarball path lets operators pick
// up the forensic bundle without re-deriving it.
type Failure struct {
	At             time.Time `json:"at"`
	Cell           string    `json:"cell"`
	Iteration      int       `json:"iteration"`
	Kind           string    `json:"kind"` // "backup" | "restore" | "verify" | "assert" | ...
	Message        string    `json:"message"`
	ReproducerPath string    `json:"reproducer_path,omitempty"`
}

// New returns a Report scaffold with the schema and timestamps
// stamped.  Callers fill in cells / faults / failures as the
// soak progresses.
func New(project string, seed int64, startedAt time.Time) *Report {
	return &Report{
		Schema:    Schema,
		Project:   project,
		Seed:      seed,
		StartedAt: startedAt,
		FleetSummary: FleetSummary{
			OSDistribution: map[string]int{},
			PGDistribution: map[string]int{},
		},
		FaultStats: FaultStats{ByPrefix: map[string]int{}},
	}
}

// Finalize stamps the end time and computes the overall verdict.
func (r *Report) Finalize(endedAt time.Time) {
	r.EndedAt = endedAt
	r.Duration = endedAt.Sub(r.StartedAt)
	r.OverallPass = len(r.Failures) == 0
	for _, c := range r.Cells {
		if !c.Pass {
			r.OverallPass = false
			break
		}
	}
}

// WriteJSON emits the JSON encoding to w.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteMarkdown emits the operator-facing summary to w.
func (r *Report) WriteMarkdown(w io.Writer) error {
	verdict := "✓ PASS"
	if !r.OverallPass {
		verdict = "✗ FAIL"
	}
	fmt.Fprintf(w, "# Soak run report — %s — %s\n\n", r.Project, verdict)
	fmt.Fprintf(w, "**Seed:** %d  \n", r.Seed)
	fmt.Fprintf(w, "**Started:** %s  \n", r.StartedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "**Ended:**   %s  \n", r.EndedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "**Duration:** %s  \n\n", r.Duration.Round(time.Second))

	fmt.Fprintln(w, "## Fleet")
	fmt.Fprintf(w, "- Cells: %d\n", r.FleetSummary.TotalCells)
	fmt.Fprintf(w, "- Containers: %d\n", r.FleetSummary.TotalContainers)
	fmt.Fprintln(w, "- OS distribution: "+kvLine(r.FleetSummary.OSDistribution))
	fmt.Fprintln(w, "- PG distribution: "+kvLine(r.FleetSummary.PGDistribution))
	fmt.Fprintln(w, "")

	fmt.Fprintln(w, "## Per-cell summary")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "| Cell | OS | PG | Arch | Role | Backups | Restores | Faults | Iter | Verdict |")
	fmt.Fprintln(w, "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
	cells := append([]CellReport{}, r.Cells...)
	sort.Slice(cells, func(i, j int) bool { return cells[i].Name < cells[j].Name })
	for _, c := range cells {
		v := "✓"
		if !c.Pass {
			v = "✗ " + c.FirstFailureMsg
		}
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %d/%d | %d/%d | %d | %d | %s |\n",
			c.Name, c.OS, c.PG, c.Arch, c.Role,
			c.BackupsTaken-c.BackupsFailed, c.BackupsTaken,
			c.RestoresAttempted-c.RestoresFailed, c.RestoresAttempted,
			c.FaultsApplied, c.LastIteration, v)
	}
	fmt.Fprintln(w, "")

	// Load measurements — only render if at least one cell
	// captured stats, so smoke runs without a sustained
	// writer don't pay for an always-empty section.
	hasStats := false
	for _, c := range cells {
		if c.LoadStats != nil {
			hasStats = true
			break
		}
	}
	if hasStats {
		fmt.Fprintln(w, "## Load measurements")
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "| Cell | Writer | WAL stream | TPS avg | p95 latency | WAL written | Stream lag | Repo lag | Segs |")
		fmt.Fprintln(w, "| --- | --- | --- | --- | --- | --- | --- | --- | --- |")
		for _, c := range cells {
			ls := c.LoadStats
			if ls == nil {
				continue
			}
			segs := "—"
			if ls.WALSegmentsCommitted > 0 {
				segs = fmt.Sprintf("%d", ls.WALSegmentsCommitted)
			}
			fmt.Fprintf(w, "| %s | %s | %s | %.0f | %.1f ms | %s | %s | %s | %s |\n",
				c.Name,
				boolMark(ls.SustainedWriterRan),
				boolMark(ls.WALStreamRan),
				ls.TPSAvg, ls.LatencyP95Ms,
				humanBytes(ls.WALBytesWritten),
				humanBytes(ls.WALStreamLagBytes),
				humanBytes(ls.WALRepoLagBytes),
				segs)
		}
		fmt.Fprintln(w, "")
	}

	fmt.Fprintln(w, "## Fault statistics")
	fmt.Fprintf(w, "- Total faults applied: %d\n", r.FaultStats.TotalApplied)
	fmt.Fprintf(w, "- Recovery failures: %d\n", r.FaultStats.RecoveryFails)
	if len(r.FaultStats.ByPrefix) > 0 {
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "| Fault | Count |")
		fmt.Fprintln(w, "| --- | --- |")
		for _, k := range sortedKeys(r.FaultStats.ByPrefix) {
			fmt.Fprintf(w, "| %s | %d |\n", k, r.FaultStats.ByPrefix[k])
		}
	}
	fmt.Fprintln(w, "")

	if len(r.Failures) > 0 {
		fmt.Fprintln(w, "## Failures")
		fmt.Fprintln(w, "")
		for _, f := range r.Failures {
			fmt.Fprintf(w, "### %s — cell %s — iteration %d\n\n",
				f.Kind, f.Cell, f.Iteration)
			fmt.Fprintf(w, "**At:** %s  \n", f.At.UTC().Format(time.RFC3339))
			fmt.Fprintf(w, "**Message:** %s  \n", f.Message)
			if f.ReproducerPath != "" {
				fmt.Fprintf(w, "**Reproducer:** [`%s`](%s)  \n", f.ReproducerPath, f.ReproducerPath)
			}
			fmt.Fprintln(w, "")
		}
	}
	return nil
}

// AddFailure appends a failure entry and ensures the affected
// cell's Pass flag is cleared.
func (r *Report) AddFailure(f Failure) {
	r.Failures = append(r.Failures, f)
	for i := range r.Cells {
		if r.Cells[i].Name == f.Cell {
			r.Cells[i].Pass = false
			if r.Cells[i].FirstFailureMsg == "" {
				r.Cells[i].FirstFailureMsg = trimMsg(f.Message)
			}
		}
	}
}

func trimMsg(s string) string {
	const max = 80
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// kvLine renders a map as "k=v, k=v" with sorted keys.
func kvLine(m map[string]int) string {
	if len(m) == 0 {
		return "(none)"
	}
	keys := sortedKeys(m)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%s=%d", k, m[k])
	}
	return sb.String()
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// boolMark renders true/false as a tight Unicode mark for the
// load-measurements table.  Falling back to "—" instead of
// "false" keeps the column narrow when most cells didn't run
// the corresponding sidecar.
func boolMark(b bool) string {
	if b {
		return "✓"
	}
	return "—"
}

// humanBytes formats a byte count as a compact iec-style
// string (e.g. 1.2 GiB).  Returns "—" for zero so empty rows
// don't render as "0 B".
func humanBytes(n int64) string {
	if n <= 0 {
		return "—"
	}
	const k = 1024
	if n < k {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(k), 0
	for x := n / k; x >= k; x /= k {
		div *= k
		exp++
	}
	unit := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}[exp]
	return fmt.Sprintf("%.2f %s", float64(n)/float64(div), unit)
}
