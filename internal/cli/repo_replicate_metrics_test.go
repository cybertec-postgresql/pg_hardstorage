package cli_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/metrics"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func replicateMetric(t *testing.T, substr string) float64 {
	t.Helper()
	var sb strings.Builder
	if err := metrics.Default().WriteExposition(&sb); err != nil {
		t.Fatalf("WriteExposition: %v", err)
	}
	for _, ln := range strings.Split(sb.String(), "\n") {
		if strings.HasPrefix(ln, "#") || !strings.Contains(ln, substr) {
			continue
		}
		f := strings.Fields(ln)
		if len(f) >= 2 {
			v, _ := strconv.ParseFloat(f[len(f)-1], 64)
			return v
		}
	}
	return 0
}

// TestRepoReplicate_EmitsEventsAndMetrics pins observability audit #1: a
// replicate run emits start/completed progress events AND increments run
// metrics — previously it was opaque (no events, no metrics, no audit).
func TestRepoReplicate_EmitsEventsAndMetrics(t *testing.T) {
	srcURL, dstURL := twoRepoDirs(t)
	plantBackupAtSrc(t, srcURL, "db1", "db1.full.A", []byte("chunk-bytes"))

	const runLine = `replicate_runs_total{result="success"}`
	before := replicateMetric(t, runLine)

	// Text mode: progress events render to the human stream; under -o json
	// they're suppressed so scripted consumers get a clean result body
	// (which is what the existing replicate tests assert).
	stdout, _, exit := runCLI(t, "repo", "replicate",
		"--from", srcURL, "--to", dstURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("repo replicate exit=%d\n%s", exit, stdout)
	}

	// Start + completion progress events are in the stream.
	for _, want := range []string{"replicate.started", "replicate.completed"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("replicate output missing %q event:\n%s", want, stdout)
		}
	}
	// The run metric incremented.
	if after := replicateMetric(t, runLine); after <= before {
		t.Errorf("replicate_runs_total{result=success} did not increment: before=%v after=%v", before, after)
	}
	// Objects copied are counted.
	if replicateMetric(t, `replicate_objects_copied_total{kind="manifest"}`) < 1 {
		t.Errorf("expected at least one manifest copied in replicate_objects_copied_total")
	}
}
