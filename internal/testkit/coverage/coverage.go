// Package coverage is the testkit's "coverage by code-path /
// matrix-cell / scenario" report.
//
// Operators ask three questions about test coverage that
// `go test -cover`'s heatmap can't answer cheanly:
//
//  1. "Which code paths are exercised by which scenarios?"
//     A regression in `internal/wal/stream/follower.go`
//     surfaces in scenarios `wal-failover-1` + `wal-failover-2`
//     but not in `restore-pitr-1`.  This view tells the
//     reviewer where to add coverage when a code path is
//     under-tested.
//
//  2. "Which (OS×PG×FS×arch) matrix cells exercise this
//     code path?"  The per-cell answer differs from the
//     per-scenario one — a scenario may run in 5 cells
//     under L4 weekly and only 1 cell under L2.
//
//  3. "How does coverage trend across releases?"  A frozen
//     snapshot per tag lets the dashboard plot a coverage
//     time-series.
//
// The package is the model + report renderer; the data
// itself is harvested by the runner (recording per-scenario
// per-cell coverage profiles into a JSON store under
// `<state>/testkit/coverage/`).
package coverage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// SchemaCoverage is the YAML / JSON schema string for v1.
// Stable across the 24-month back-compat window.
const SchemaCoverage = "pg_hardstorage.testkit.coverage.v1"

// Profile is one scenario's coverage harvest in one matrix
// cell.  Files maps code-path → percentage of statements
// covered (0..100).
type Profile struct {
	Schema      string             `json:"schema"`
	Scenario    string             `json:"scenario"`
	MatrixCell  string             `json:"matrix_cell"`
	HarvestedAt time.Time          `json:"harvested_at"`
	Files       map[string]float64 `json:"files"`
}

// Report is the aggregated view across many profiles.
type Report struct {
	Schema      string                       `json:"schema"`
	GeneratedAt time.Time                    `json:"generated_at"`
	ByFile      map[string]*FileCoverage     `json:"by_file"`
	ByScenario  map[string]*ScenarioCoverage `json:"by_scenario"`
	Cells       []string                     `json:"matrix_cells"`
}

// FileCoverage is one Go file's aggregated coverage across
// every harvested profile.
type FileCoverage struct {
	Path           string             `json:"path"`
	MaxPct         float64            `json:"max_pct"`   // best coverage seen across cells
	AvgPct         float64            `json:"avg_pct"`   // mean across cells
	ScenarioPcts   map[string]float64 `json:"scenarios"` // scenario → max-coverage-of-this-file
	CellsExercised int                `json:"cells_exercised"`
}

// ScenarioCoverage is one scenario's contribution.
type ScenarioCoverage struct {
	Name        string   `json:"name"`
	Files       int      `json:"files"`
	AvgFilePct  float64  `json:"avg_file_pct"`
	MatrixCells []string `json:"matrix_cells"`
}

// Aggregate fold N profiles into one Report.
func Aggregate(profiles []Profile) *Report {
	r := &Report{
		Schema:      SchemaCoverage,
		GeneratedAt: time.Now().UTC(),
		ByFile:      map[string]*FileCoverage{},
		ByScenario:  map[string]*ScenarioCoverage{},
	}
	cellSet := map[string]struct{}{}

	for _, p := range profiles {
		cellSet[p.MatrixCell] = struct{}{}
		sc, ok := r.ByScenario[p.Scenario]
		if !ok {
			sc = &ScenarioCoverage{Name: p.Scenario}
			r.ByScenario[p.Scenario] = sc
		}
		var sumPct float64
		for file, pct := range p.Files {
			fc, ok := r.ByFile[file]
			if !ok {
				fc = &FileCoverage{
					Path:         file,
					ScenarioPcts: map[string]float64{},
				}
				r.ByFile[file] = fc
			}
			if pct > fc.MaxPct {
				fc.MaxPct = pct
			}
			fc.AvgPct += pct
			fc.CellsExercised++
			if existing, ok := fc.ScenarioPcts[p.Scenario]; !ok || pct > existing {
				fc.ScenarioPcts[p.Scenario] = pct
			}
			sumPct += pct
		}
		if len(p.Files) > 0 {
			sc.AvgFilePct = (sc.AvgFilePct*float64(sc.Files) + sumPct) / float64(sc.Files+len(p.Files))
			sc.Files += len(p.Files)
		}
		if !contains(sc.MatrixCells, p.MatrixCell) {
			sc.MatrixCells = append(sc.MatrixCells, p.MatrixCell)
			sort.Strings(sc.MatrixCells)
		}
	}

	for _, fc := range r.ByFile {
		if fc.CellsExercised > 0 {
			fc.AvgPct = fc.AvgPct / float64(fc.CellsExercised)
		}
	}

	for c := range cellSet {
		r.Cells = append(r.Cells, c)
	}
	sort.Strings(r.Cells)
	return r
}

// LoadProfiles reads NDJSON-encoded profiles from r.  Each
// line is one Profile.  Useful for the runner-emitted store
// under <state>/testkit/coverage/<scenario>.<cell>.ndjson.
func LoadProfiles(r io.Reader) ([]Profile, error) {
	dec := json.NewDecoder(r)
	var out []Profile
	for dec.More() {
		var p Profile
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("coverage: decode profile: %w", err)
		}
		if p.Schema != "" && p.Schema != SchemaCoverage {
			return nil, fmt.Errorf("coverage: unsupported profile schema %q", p.Schema)
		}
		out = append(out, p)
	}
	return out, nil
}

// WriteText renders the report in the testkit's text format
// (a punch-list grouped by file, lowest-coverage first —
// where the operator should add tests).
func (r *Report) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "Coverage report  generated %s\n", r.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "Matrix cells:    %s\n", strings.Join(r.Cells, ", "))
	fmt.Fprintf(bw, "Files tracked:   %d\n", len(r.ByFile))
	fmt.Fprintf(bw, "Scenarios:       %d\n\n", len(r.ByScenario))

	type row struct {
		path string
		pct  float64
		scen int
	}
	rows := make([]row, 0, len(r.ByFile))
	for path, fc := range r.ByFile {
		rows = append(rows, row{path: path, pct: fc.MaxPct, scen: len(fc.ScenarioPcts)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].pct < rows[j].pct })

	fmt.Fprintf(bw, "Lowest-coverage files (best across cells):\n")
	const top = 10
	for i, r := range rows {
		if i >= top {
			break
		}
		fmt.Fprintf(bw, "  %5.1f%%  %s  (in %d scenarios)\n", r.pct, r.path, r.scen)
	}
	if len(rows) > top {
		fmt.Fprintf(bw, "  ... +%d more files\n", len(rows)-top)
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

// FilesByScenario inverts the report: returns scenario name
// → sorted list of files it exercises.  Used by `testkit
// coverage report --by scenario`.
func (r *Report) FilesByScenario() map[string][]string {
	out := map[string][]string{}
	for path, fc := range r.ByFile {
		for scenario := range fc.ScenarioPcts {
			out[scenario] = append(out[scenario], path)
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// ScenariosByFile inverts the other way: file path →
// scenarios that exercise it.  Used to answer "what
// scenario should I add to cover this regression?"
func (r *Report) ScenariosByFile() map[string][]string {
	out := map[string][]string{}
	for path, fc := range r.ByFile {
		for scenario := range fc.ScenarioPcts {
			out[path] = append(out[path], scenario)
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// ErrEmpty is returned by Aggregate when no profiles are
// supplied.  Callers turn this into a "no coverage data
// recorded yet" message.
var ErrEmpty = errors.New("coverage: no profiles to aggregate")

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
