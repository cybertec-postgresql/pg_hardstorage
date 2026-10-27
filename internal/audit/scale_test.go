package audit

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"encoding/json"
	"net/url"
	"path/filepath"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

func vmHWM(t testing.TB) uint64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skip("no /proc/self/status")
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(ln, "VmHWM:") {
			f := strings.Fields(ln)
			kb, _ := strconv.ParseUint(f[1], 10, 64)
			return kb << 10
		}
	}
	t.Skip("no VmHWM")
	return 0
}

// TestScale_AuditChainPeakRSS appends PGHS_SCALE_EVENTS events (default 300k)
// and then runs the full-chain walks (verify-chain, summary, a limited
// search, verify-anchor's sequence lookup) under a soft memory limit,
// asserting the walks' peak RSS stays bounded: they stream the chain and
// keep linkage tuples in an external sorter instead of slices of every key
// and tuple. Opt-in: PGHS_SCALE=1.
func TestScale_AuditChainPeakRSS(t *testing.T) {
	if os.Getenv("PGHS_SCALE") != "1" {
		t.Skip("set PGHS_SCALE=1 to run the audit-chain peak-RSS proof")
	}
	n := 300_000
	if v := os.Getenv("PGHS_SCALE_EVENTS"); v != "" {
		n, _ = strconv.Atoi(v)
	}
	debug.SetMemoryLimit(256 << 20)
	defer debug.SetMemoryLimit(-1)

	// Build the chain by writing event files directly (Append fsyncs twice
	// per event, which would make a million-event fixture take hours); the
	// layout, hashes and head pointer are the real ones.
	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	store := NewStore(sp)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(shardEventDir(""))), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := GenesisHash
	var last *Event
	for i := 0; i < n; i++ {
		ev := &Event{Schema: Schema, Action: fmt.Sprintf("scale.event.%d", i%7), Actor: "scale",
			Timestamp: time.Unix(1_700_000_000+int64(i), 0).UTC(), ID: fmt.Sprintf("ev-%09d", i),
			Sequence: int64(i), PrevHash: prev}
		h, err := ComputeHash(ev)
		if err != nil {
			t.Fatal(err)
		}
		ev.Hash, prev = h, h
		body, _ := json.Marshal(ev)
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(keyFor(ev))), body, 0o644); err != nil {
			t.Fatal(err)
		}
		last = ev
	}
	if err := store.writeHeadPointer(ctx, "", last, keyFor(last)); err != nil {
		t.Fatal(err)
	}
	base := vmHWM(t)
	t.Logf("after appending %d events: peak RSS %d MiB", n, base>>20)

	res, err := store.VerifyChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.EventsChecked != n {
		t.Fatalf("VerifyChain ok=%v checked=%d want %d", res.OK, res.EventsChecked, n)
	}
	counts, total, err := store.SummaryByAction(ctx, ListFilters{})
	if err != nil || total != n || len(counts) != 7 {
		t.Fatalf("SummaryByAction total=%d groups=%d err=%v", total, len(counts), err)
	}
	evs, err := store.Search(ctx, ListFilters{Limit: 10, Reverse: true})
	if err != nil || len(evs) != 10 {
		t.Fatalf("Search limit: %d events, err=%v", len(evs), err)
	}
	got := vmHWM(t)
	t.Logf("after verify-chain + summary + search over %d events: peak RSS %d MiB", n, got>>20)
	// Per-event linear state would be >= ~200 B/event (200 MiB at 1M events
	// and growing); allow a fixed 160 MiB over the append phase's peak, which
	// covers the sorter's buffer and GC headroom regardless of n.
	if got > base+160<<20 {
		t.Errorf("walks raised peak RSS from %d to %d MiB over %d events", base>>20, got>>20, n)
	}
}
