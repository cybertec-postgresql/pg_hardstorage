package validate

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
)

// StopSustainedLoad gives the supervisor 10 s to wind down and then
// reads the captured progress stream anyway. A `docker exec` whose
// output pipe outlives the killed docker client (here: a child that
// inherited stderr and keeps writing, as a wedged daemon attach does)
// keeps exec's copier goroutine writing into that buffer while Stop
// reads it — a data race `go test -race` reports.
func TestStopSustainedLoadDoesNotRaceALingeringWriter(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	fake := writeFakeDocker(t, `case "$*" in
  *" bash -c "*) echo /usr/bin; exit 0 ;;
  *psql*) echo 0/1000000; exit 0 ;;
  *pgbench*)
    ( while :; do echo "progress: 10.0 s, 5.0 tps, lat 1.0 ms stddev 0.1" >&2; sleep 0.005; done ) &
    echo $! > `+pidFile+`
    wait ;;
esac
exit 0
`)
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	old := sustainedStopTimeout
	sustainedStopTimeout = 200 * time.Millisecond
	defer func() { sustainedStopTimeout = old }()

	d := &DockerCellRuntime{CellName: "c", Container: "cell-c", DockerBin: fake,
		PGUser: "postgres", PGDatabase: "postgres",
		Profile: config.Profile{SustainedClients: 1}}
	if err := d.StartSustainedLoad(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	stats, err := d.StopSustainedLoad(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats == nil || stats.TPSAvg == 0 {
		t.Fatalf("progress samples were not captured: %+v", stats)
	}
	// Keep reading while the orphan writes: with an unsynchronised
	// buffer the race detector flags the copier's write against Stop's
	// read above; give it a window to overlap.
	time.Sleep(100 * time.Millisecond)
}
