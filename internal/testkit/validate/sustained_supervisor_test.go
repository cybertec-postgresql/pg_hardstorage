package validate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
)

func TestParsePgbenchProgress(t *testing.T) {
	out := `pgbench (17.11)
progress: 10.0 s, 500.0 tps, lat 30.000 ms stddev 5.000, 0 failed
progress: 20.0 s, 700.0 tps, lat 20.000 ms stddev 4.000, 0 failed
pgbench: error: client 3 aborted in command 4 (SQL) of script 0; FATAL:  terminating connection due to administrator command
progress: 10.0 s, 600.0 tps, lat 50.000 ms stddev 9.000
`
	tps, p95, n := parsePgbenchProgress(out)
	if n != 3 {
		t.Fatalf("samples = %d, want 3 (across two runs, both line formats)", n)
	}
	if tps != 600 {
		t.Errorf("tps avg = %v, want 600", tps)
	}
	if p95 != 50 {
		t.Errorf("p95 of window latencies = %v, want 50", p95)
	}
	if _, _, n := parsePgbenchProgress("pgbench: error: connection refused"); n != 0 {
		t.Error("no progress lines, no samples")
	}
}

// TestSustainedWriterIsSupervised is the regression for the heavy
// soak's silent gap: pgbench exits whenever a fault kills PostgreSQL,
// and the writer used to stay dead for the rest of the run while the
// report said "Writer ✓". A fake docker whose pgbench prints progress
// and then exits models exactly that; the supervisor must restart it,
// count the restarts, and aggregate progress across every run.
func TestSustainedWriterIsSupervised(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "docker")
	script := `#!/bin/sh
case "$*" in
  *bash*) echo /usr/lib/postgresql/17/bin ;;
  *pg_current_wal_lsn*) echo 0/3000000 ;;
  *pg_wal_lsn_diff*) echo 16777216 ;;
  *pgbench*)
    echo "progress: 10.0 s, 400.0 tps, lat 40.000 ms stddev 1.0, 0 failed" >&2
    exit 2 ;;   # a fault killed PostgreSQL; every client aborted
esac
exit 0
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := sustainedRestartDelay
	sustainedRestartDelay = 20 * time.Millisecond
	defer func() { sustainedRestartDelay = old }()

	d := &DockerCellRuntime{CellName: "c", Container: "cell-c", DockerBin: fake,
		PGUser: "postgres", PGDatabase: "postgres",
		Profile: config.Profile{SustainedClients: 4}}
	ctx := context.Background()
	if err := d.StartSustainedLoad(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	stats, err := d.StopSustainedLoad(ctx)
	if err != nil || stats == nil {
		t.Fatalf("stop: %v %v", stats, err)
	}
	if stats.SustainedWriterRestarts < 3 {
		t.Errorf("restarts = %d; a writer that dies must be restarted, repeatedly", stats.SustainedWriterRestarts)
	}
	if stats.TPSAvg != 400 {
		t.Errorf("tps = %v, want 400 aggregated across runs (pgbench's end summary never prints when it is killed)", stats.TPSAvg)
	}
	if stats.SustainedWriterUptimePct <= 0 {
		t.Error("uptime must be reported from progress samples")
	}
	if stats.WALBytesWritten != 16777216 {
		t.Errorf("WAL written = %d, want the LSN distance PostgreSQL computed", stats.WALBytesWritten)
	}
}
