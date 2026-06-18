package coverage_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/coverage"
)

func TestAggregate_Basic(t *testing.T) {
	profiles := []coverage.Profile{
		{
			Schema:      coverage.SchemaCoverage,
			Scenario:    "wal-failover-1",
			MatrixCell:  "ubuntu-22.04/pg-17/ext4",
			HarvestedAt: time.Now().UTC(),
			Files: map[string]float64{
				"internal/wal/stream/follower.go": 80.0,
				"internal/restore/restore.go":     20.0,
			},
		},
		{
			Schema:      coverage.SchemaCoverage,
			Scenario:    "restore-pitr-1",
			MatrixCell:  "ubuntu-22.04/pg-17/ext4",
			HarvestedAt: time.Now().UTC(),
			Files: map[string]float64{
				"internal/restore/restore.go": 95.0,
				"internal/restore/preview.go": 60.0,
			},
		},
	}
	r := coverage.Aggregate(profiles)
	if len(r.ByFile) != 3 {
		t.Errorf("ByFile size = %d, want 3", len(r.ByFile))
	}
	if r.ByFile["internal/restore/restore.go"].MaxPct != 95.0 {
		t.Errorf("MaxPct should pick the higher of the two; got %v", r.ByFile["internal/restore/restore.go"].MaxPct)
	}
	if !contains(r.ByScenario["restore-pitr-1"].MatrixCells, "ubuntu-22.04/pg-17/ext4") {
		t.Errorf("Scenario didn't capture cell")
	}
}

func TestAggregate_FilesByScenario(t *testing.T) {
	profiles := []coverage.Profile{
		{Scenario: "alpha", MatrixCell: "x", Files: map[string]float64{"a.go": 50, "b.go": 60}},
		{Scenario: "beta", MatrixCell: "x", Files: map[string]float64{"b.go": 70, "c.go": 80}},
	}
	r := coverage.Aggregate(profiles)
	by := r.FilesByScenario()
	if len(by["alpha"]) != 2 {
		t.Errorf("alpha files = %v", by["alpha"])
	}
	if !contains(by["alpha"], "a.go") || !contains(by["alpha"], "b.go") {
		t.Errorf("alpha missing expected files: %v", by["alpha"])
	}
	if !contains(by["beta"], "c.go") {
		t.Errorf("beta missing c.go: %v", by["beta"])
	}
}

func TestAggregate_ScenariosByFile(t *testing.T) {
	profiles := []coverage.Profile{
		{Scenario: "alpha", MatrixCell: "x", Files: map[string]float64{"a.go": 50}},
		{Scenario: "beta", MatrixCell: "x", Files: map[string]float64{"a.go": 70}},
	}
	r := coverage.Aggregate(profiles)
	by := r.ScenariosByFile()
	if len(by["a.go"]) != 2 {
		t.Errorf("a.go scenarios = %v", by["a.go"])
	}
}

func TestLoadProfiles_NDJSON(t *testing.T) {
	body := `{"schema":"pg_hardstorage.testkit.coverage.v1","scenario":"x","matrix_cell":"y","files":{"a.go":50}}
{"schema":"pg_hardstorage.testkit.coverage.v1","scenario":"y","matrix_cell":"y","files":{"b.go":75}}
`
	profiles, err := coverage.LoadProfiles(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 {
		t.Errorf("expected 2 profiles, got %d", len(profiles))
	}
}

func TestLoadProfiles_RejectsBadSchema(t *testing.T) {
	body := `{"schema":"some.other.schema","scenario":"x","matrix_cell":"y","files":{}}`
	if _, err := coverage.LoadProfiles(strings.NewReader(body)); err == nil {
		t.Error("expected schema rejection")
	}
}

func TestReport_WriteText(t *testing.T) {
	profiles := []coverage.Profile{
		{Scenario: "alpha", MatrixCell: "linux", Files: map[string]float64{"low.go": 20.0, "high.go": 95.0}},
	}
	r := coverage.Aggregate(profiles)
	var buf bytes.Buffer
	if err := r.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"Coverage report", "low.go", "Lowest-coverage files"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q\n%s", want, out)
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
