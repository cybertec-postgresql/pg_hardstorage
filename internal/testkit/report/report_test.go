package report_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/report"
)

func sampleReport(t *testing.T) *report.Report {
	t.Helper()
	r := report.New("pgvalidate-test", 42, time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))
	r.FleetSummary.TotalCells = 2
	r.FleetSummary.TotalContainers = 4
	r.FleetSummary.OSDistribution["ubuntu:24.04"] = 1
	r.FleetSummary.OSDistribution["rockylinux:9"] = 1
	r.FleetSummary.PGDistribution["17"] = 1
	r.FleetSummary.PGDistribution["16"] = 1
	r.Cells = []report.CellReport{
		{Name: "u24-pg17", OS: "ubuntu:24.04", PG: "17", Arch: "amd64", Role: "standalone",
			BackupsTaken: 12, RestoresAttempted: 4, FaultsApplied: 6,
			IterationsRun: 24, LastIteration: 24, Pass: true},
		{Name: "rocky9-pg16", OS: "rockylinux:9", PG: "16", Arch: "amd64", Role: "standalone",
			BackupsTaken: 5, BackupsFailed: 1, RestoresAttempted: 2, FaultsApplied: 3,
			IterationsRun: 10, LastIteration: 10, Pass: true},
	}
	r.FaultStats.TotalApplied = 9
	r.FaultStats.ByPrefix["signal"] = 4
	r.FaultStats.ByPrefix["disk_full"] = 3
	r.FaultStats.ByPrefix["toxiproxy"] = 2
	return r
}

func TestReport_FinalizePassPath(t *testing.T) {
	r := sampleReport(t)
	r.Finalize(r.StartedAt.Add(2 * time.Hour))
	if !r.OverallPass {
		t.Errorf("expected pass with no failures + all cells Pass=true")
	}
	if r.Duration != 2*time.Hour {
		t.Errorf("duration: %v", r.Duration)
	}
}

func TestReport_FinalizeFailPath_Failure(t *testing.T) {
	r := sampleReport(t)
	r.AddFailure(report.Failure{
		At: r.StartedAt.Add(time.Hour), Cell: "u24-pg17", Iteration: 14,
		Kind: "verify", Message: "pg_verifybackup exited 1: missing chunk",
	})
	r.Finalize(r.StartedAt.Add(2 * time.Hour))
	if r.OverallPass {
		t.Errorf("expected fail with one failure")
	}
	// AddFailure should have flipped the cell's Pass flag.
	for _, c := range r.Cells {
		if c.Name == "u24-pg17" && c.Pass {
			t.Errorf("cell should be marked failed")
		}
	}
}

func TestReport_FinalizeFailPath_CellPassFalse(t *testing.T) {
	r := sampleReport(t)
	r.Cells[1].Pass = false
	r.Cells[1].FirstFailureMsg = "agent OOM"
	r.Finalize(r.StartedAt.Add(time.Hour))
	if r.OverallPass {
		t.Errorf("expected fail when any cell has Pass=false")
	}
}

func TestReport_WriteJSON_RoundTrip(t *testing.T) {
	r := sampleReport(t)
	r.Finalize(r.StartedAt.Add(time.Hour))
	var buf bytes.Buffer
	if err := r.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var back report.Report
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Schema != report.Schema {
		t.Errorf("schema lost: %q", back.Schema)
	}
	if len(back.Cells) != len(r.Cells) {
		t.Errorf("cells lost: %d vs %d", len(back.Cells), len(r.Cells))
	}
}

func TestReport_WriteMarkdown_HasExpectedSections(t *testing.T) {
	r := sampleReport(t)
	r.AddFailure(report.Failure{
		At: r.StartedAt.Add(30 * time.Minute), Cell: "u24-pg17", Iteration: 7,
		Kind: "restore", Message: "restore failed mid-flight",
		ReproducerPath: "failures/u24-pg17.tar.gz",
	})
	r.Finalize(r.StartedAt.Add(time.Hour))
	var buf bytes.Buffer
	if err := r.WriteMarkdown(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, fragment := range []string{
		"# Soak run report", "✗ FAIL",
		"**Seed:** 42",
		"## Fleet",
		"## Per-cell summary",
		"## Fault statistics",
		"| signal | 4 |",
		"## Failures",
		"### restore — cell u24-pg17 — iteration 7",
		"failures/u24-pg17.tar.gz",
	} {
		if !strings.Contains(out, fragment) {
			t.Errorf("markdown missing %q\n---\n%s", fragment, out)
		}
	}
}

func TestReport_PassVerdictInMarkdown(t *testing.T) {
	r := sampleReport(t)
	r.Finalize(r.StartedAt.Add(time.Hour))
	var buf bytes.Buffer
	_ = r.WriteMarkdown(&buf)
	if !strings.Contains(buf.String(), "✓ PASS") {
		t.Errorf("expected PASS verdict")
	}
}
