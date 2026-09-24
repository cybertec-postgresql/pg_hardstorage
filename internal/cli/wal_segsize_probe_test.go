package cli

// probeSegmentSize must never ASSUME a wal_segment_size. Segment size
// determines segment NAMES, so on a cluster built with
// `initdb --wal-segsize 64MB` an assumed 16 MiB names every archived
// segment wrongly — and on a FRESH deployment guardSegmentSize has no
// archived WAL to contradict it, so nothing notices until a restore.
//
// It used to assume exactly that on a connect failure, quietly, on the
// theory that streamAttempt would hit the same failure and retry. It
// did — but the probe ran once, at startup, and was never repeated: a
// PostgreSQL that was down for the startup probe and up for the first
// reconnect streamed its whole life at 16 MiB. The probe now fails, and
// verifyStreamSource re-runs it on every attempt against the cluster
// that attempt reached (TestIntegration_WalStream_SegSizeProbedOnReconnect
// drives that end to end).

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func TestProbeSegmentSize_ConnectFailureIsAnErrorNotAnAssumption(t *testing.T) {
	// Port 1 refuses immediately; connect_timeout keeps it fast even
	// where it does not.
	dsn := "postgres://nobody@127.0.0.1:1/nodb?connect_timeout=1&sslmode=disable"
	got, err := probeSegmentSize(context.Background(), dsn)
	if err == nil {
		t.Fatalf("probe against an unreachable cluster returned %d bytes and no error.\n\n"+
			"That is an assumed size, and the stream would chop and NAME segments with it; "+
			"a 64 MiB cluster's archive comes out unrestorable.", got)
	}
	// The failure must stay on the RETRY path: a PostgreSQL that is down
	// for a moment is the ordinary case the reconnect loop exists for.
	if isPermanentStreamSetupError(err) {
		t.Errorf("connect failure %v classified permanent; the reconnect loop must retry it", err)
	}
	if _, ok := output.AsOutputError(err); !ok {
		t.Errorf("err = %T, want a structured *output.Error", err)
	}
}

// The query-failure branch needs a cluster that accepts a connection and
// then refuses the setting, which no unit test can stand up. Assert at
// the source level that nothing after the query returns the default.
func TestProbeSegmentSize_QueryFailureDoesNotAssumeTheDefault(t *testing.T) {
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
	if !strings.Contains(fn, "QueryWALSegmentSize(") {
		t.Fatal("probeSegmentSize no longer queries wal_segment_size")
	}
	if strings.Contains(fn, "DefaultSegmentSize") {
		t.Error("probeSegmentSize refers to walsink.DefaultSegmentSize: it must return what the " +
			"cluster reports or an error, never a default.")
	}
}

// The permanent refusals of the per-attempt guards must stop the
// reconnect loop; retrying cannot change which cluster the DSN reaches.
func TestVerifyStreamSourceRefusalsArePermanent(t *testing.T) {
	for _, code := range []string{
		"preflight.system_identifier_changed",
		"wal.system_identifier_changed",
		"preflight.wal_segment_size",
		"preflight.wal_segment_size_changed",
	} {
		if !isPermanentStreamSetupError(output.NewError(code, "x")) {
			t.Errorf("%s is retried; the reconnect loop would repeat the refusal forever", code)
		}
	}
	if isPermanentStreamSetupError(output.NewError("wal.segment_size_probe_failed", "x")) {
		t.Error("wal.segment_size_probe_failed is permanent; a failed read on a connected " +
			"cluster is transient and must be retried")
	}
}
