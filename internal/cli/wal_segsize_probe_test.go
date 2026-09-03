package cli

// probeSegmentSize assumes 16 MiB when it cannot read the cluster's
// wal_segment_size. That assumption is load-bearing: segment size
// determines segment NAMES, so on a cluster built with
// `initdb --wal-segsize 64MB` the assumption names every archived
// segment wrongly. guardSegmentSize catches that when the deployment
// already has WAL to compare against — but on a FRESH deployment there
// is nothing to compare, so the only thing standing between the
// operator and an unrestorable archive is being told the assumption was
// made.
//
// The two fallbacks are not the same and must not behave the same:
//
//   - CONNECT failure: streamAttempt is about to hit the identical
//     failure with retry/backoff, so the stream never proceeds on the
//     assumption. Warning here would be noise on every transient blip.
//   - QUERY failure on a CONNECTED cluster: the stream DOES proceed on
//     the assumption. That one must be reported.
//
// Only the connect branch is reachable without a live PostgreSQL, so
// that is what this test pins. The query branch is asserted structurally
// below instead — see TestProbeSegmentSize_QueryFailureWarns.

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	rendererjson "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/json"
)

func TestProbeSegmentSize_ConnectFailureFallsBackQuietly(t *testing.T) {
	var stdout, stderr bytes.Buffer
	d := output.NewDispatcher(rendererjson.New(), &stdout, &stderr)

	// Port 1 refuses immediately; connect_timeout keeps it fast even
	// where it does not.
	dsn := "postgres://nobody@127.0.0.1:1/nodb?connect_timeout=1&sslmode=disable"
	got, err := probeSegmentSize(context.Background(), d, dsn)
	if err != nil {
		t.Fatalf("a connect failure must not fail the probe: %v", err)
	}
	if got != walsink.DefaultSegmentSize {
		t.Errorf("fallback = %d, want %d", got, int64(walsink.DefaultSegmentSize))
	}
	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, "segment_size_probe_failed") {
		t.Errorf("a connect failure emitted the assumption warning.\n\n"+
			"streamAttempt is about to report the same connect failure with retry and "+
			"backoff, so the stream does not proceed on the assumption; warning here "+
			"would fire on every transient blip and train the operator to ignore it.\n%s",
			combined)
	}
}

// The query-failure branch needs a cluster that accepts a connection and
// then refuses the setting, which no unit test can stand up. Assert the
// wiring at the source level instead — the same approach
// TestWalStream_CallsItsPreflightGuards uses, and for the same reason:
// the alternative is claiming coverage that does not exist.
func TestProbeSegmentSize_QueryFailureWarns(t *testing.T) {
	src, err := os.ReadFile("wal.go")
	if err != nil {
		t.Fatalf("read wal.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func probeSegmentSize(")
	if start < 0 {
		t.Fatal("probeSegmentSize not found; this guard needs updating, not deleting")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not delimit probeSegmentSize")
	}
	fn := body[start : start+end]

	qIdx := strings.Index(fn, "QueryWALSegmentSize(")
	if qIdx < 0 {
		t.Fatal("probeSegmentSize no longer queries wal_segment_size")
	}
	// Everything after the query is the branch that decides what to do
	// with a failure to read the setting.
	tail := fn[qIdx:]
	if !strings.Contains(tail, "segment_size_probe_failed") {
		t.Error("probeSegmentSize falls back to the default after a failed " +
			"wal_segment_size query without emitting segment_size_probe_failed.\n\n" +
			"The stream then proceeds on an assumption that names every segment, and on " +
			"a fresh deployment guardSegmentSize has no archived WAL to contradict it. " +
			"The operator finds out at restore.")
	}
}
