package repo_test

import (
	"context"
	"fmt"
	"io"
	"iter"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// walStore wraps a plugin to count WAL manifest reads and, optionally, to
// list in an adversarial order.
type walStore struct {
	storage.StoragePlugin
	walGets atomic.Int64
	reverse bool
}

func (w *walStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if strings.HasPrefix(key, "wal/") {
		w.walGets.Add(1)
	}
	return w.StoragePlugin.Get(ctx, key)
}

func (w *walStore) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	if !w.reverse {
		return w.StoragePlugin.List(ctx, prefix)
	}
	return func(yield func(storage.ObjectInfo, error) bool) {
		var all []storage.ObjectInfo
		for info, err := range w.StoragePlugin.List(ctx, prefix) {
			if err != nil {
				yield(storage.ObjectInfo{}, err)
				return
			}
			all = append(all, info)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Key > all[j].Key })
		for _, info := range all {
			if !yield(info, nil) {
				return
			}
		}
	}
}

func plantWALRun(t *testing.T, sp storage.StoragePlugin, n int) {
	t.Helper()
	plantBackupManifest(t, sp, "db1", "db1.full.1", "0/0A000000", time.Now())
	for i := 1; i <= n; i++ {
		plantWALSegManifest(t, sp, "db1", 1, fmt.Sprintf("%024X", i),
			fmt.Sprintf("0/%08X", i*0x1000000), time.Now().Add(-48*time.Hour), []int64{10})
	}
}

// The prune walks WAL as a stream and, on an ordered listing, reads only the
// prunable prefix (+1): the kept suffix is proven from the ordering alone.
func TestWALPrune_StreamReadsOnlyThePrunablePrefix(t *testing.T) {
	_, base := newTestRepo(t)
	defer base.Close()
	const n = 40
	plantWALRun(t, base, n)
	sp := &walStore{StoragePlugin: base}
	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{Deployment: "db1"})
	if err != nil {
		t.Fatal(err)
	}
	// Segments 1..9 end below the 0/0A000000 frontier.
	if res.SegmentsConsidered != n || res.SegmentsDeleted != 9 || res.SegmentsKept != n-9 {
		t.Fatalf("considered=%d deleted=%d kept=%d", res.SegmentsConsidered, res.SegmentsDeleted, res.SegmentsKept)
	}
	if got := sp.walGets.Load(); got != 10 { // 9 deleted + the first kept one
		t.Errorf("read %d WAL manifests, want 10 (prefix + boundary), not all %d", got, n)
	}
	for i := 1; i <= n; i++ {
		exists := segExists(t, base, "db1", 1, fmt.Sprintf("%024X", i))
		if want := i >= 10; exists != want {
			t.Errorf("segment %d exists=%v, want %v", i, exists, want)
		}
	}
}

// A backend that lists out of order must still prune the right segments:
// the ordering shortcut only ever keeps more, and switches itself off.
func TestWALPrune_UnorderedListingPrunesTheSameSet(t *testing.T) {
	_, base := newTestRepo(t)
	defer base.Close()
	const n = 30
	plantWALRun(t, base, n)
	sp := &walStore{StoragePlugin: base, reverse: true}
	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{Deployment: "db1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.SegmentsConsidered != n || res.SegmentsFailed != 0 {
		t.Fatalf("considered=%d failed=%d", res.SegmentsConsidered, res.SegmentsFailed)
	}
	// Reverse order meets the kept suffix first; the first key out of order
	// disables the shortcut, so segments 1..9 are still found and deleted.
	// (Only the very first listed key is judged before disorder is seen.)
	for i := 1; i <= 9; i++ {
		if segExists(t, base, "db1", 1, fmt.Sprintf("%024X", i)) {
			t.Errorf("segment %d should have been pruned", i)
		}
	}
	for i := 10; i <= n; i++ {
		if !segExists(t, base, "db1", 1, fmt.Sprintf("%024X", i)) {
			t.Errorf("segment %d must be kept", i)
		}
	}
}
