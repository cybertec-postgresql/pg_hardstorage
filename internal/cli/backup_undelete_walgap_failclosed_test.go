package cli

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

// failTimelineListSP fails listing the timeline's segment manifests —
// the WAL-coverage probe cannot run.
type failTimelineListSP struct{ storage.StoragePlugin }

func (s failTimelineListSP) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	if strings.HasPrefix(prefix, "wal/db1/0000") {
		return func(yield func(storage.ObjectInfo, error) bool) {
			yield(storage.ObjectInfo{}, errors.New("backend: 503 slow down"))
		}
	}
	return s.StoragePlugin.List(ctx, prefix)
}

// A WAL-coverage probe that ERRORED was treated as "coverage intact":
// no gap record, so the restore preflights could not refuse a
// --to-latest that would later truncate silently at a pruned hole. A
// probe that cannot answer must fail closed — record a conservative gap
// from the backup's stop, loudly.
func TestRecordResurrectedWALGap_ProbeErrorFailsClosed(t *testing.T) {
	base, _ := newFsRepo(t)
	plantWALSeg(t, base, "db1", 1, 8)
	sp := failTimelineListSP{base}
	d, buf := captureDispatcher(t)

	got := recordResurrectedWALGap(context.Background(), d, sp, "db1", "b1", resurrectedManifest("0/3000000"))
	if got == "" || !strings.HasPrefix(got, "0/3000000..") {
		t.Fatalf("probe error treated as intact coverage (window=%q)\n%s", got, buf.String())
	}
	recs, err := gapstate.New(base).List(context.Background(), "db1")
	if err != nil || len(recs) != 1 || recs[0].GapStartLSN != "0/3000000" {
		t.Fatalf("no conservative gap record persisted: %+v %v", recs, err)
	}
	if !strings.Contains(buf.String(), "probe_error") {
		t.Errorf("the fail-closed record must say why:\n%s", buf.String())
	}
}
