package restore_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/metrics"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func restoreMetric(t *testing.T, substr string) float64 {
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

// TestRestore_EmitsMetrics pins observability audit #2: a restore emits
// started + completed{result}/duration metrics (previously restore had
// none, while backup was fully metered).
func TestRestore_EmitsMetrics(t *testing.T) {
	fx := newFixture(t)
	const completed = `restore_completed_total{deployment="db1",result="success"}`
	beforeCompleted := restoreMetric(t, completed)
	beforeStarted := restoreMetric(t, `restore_started_total{deployment="db1"}`)

	if _, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   "db1.full.20260428T130000Z.0001",
		TargetDir:  t.TempDir() + "/restored",
		Verifier:   fx.verifier,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if after := restoreMetric(t, completed); after <= beforeCompleted {
		t.Fatalf("restore_completed_total{result=success} did not increment: before=%v after=%v", beforeCompleted, after)
	}
	if after := restoreMetric(t, `restore_started_total{deployment="db1"}`); after <= beforeStarted {
		t.Errorf("restore_started_total did not increment: before=%v after=%v", beforeStarted, after)
	}
}
