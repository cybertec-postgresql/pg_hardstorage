package cli

// wal_segment_size_test.go — one unreadable object decided the
// cluster's segment size.
//
// scanWALSegments derives every segment NUMBER from the cluster's
// wal_segment_size:
//
//	segNum = logID * (4 GiB / size) + segInLog
//
// It learned that size by opening exactly ONE manifest — whichever the
// backend happened to list first — and silently fell back to the 16 MiB
// default if that single Get, read or parse failed. A transient 503 or
// one torn object therefore decided the numbering for the whole
// command, and the fallback is not a small error:
//
//   - on a 64 MiB cluster, assuming 16 MiB spreads consecutive segments
//     across a 256-stride grid, so `wal audit` reports a fabricated
//     192-segment gap at every log-id boundary — exit 9 plus a
//     wal.gap_detected event written into the audit chain, on every
//     cron run;
//   - on a 1 MiB cluster the assumption COLLIDES distinct segments onto
//     one number, which can mask a real gap. That direction is worse,
//     and it is silent.
//
// The size is cluster-wide and constant, so any readable manifest
// answers it. Probing several costs nothing and stops one bad object
// deciding.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/faultinject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

func TestDeploymentSegmentSize_OneUnreadableManifestDoesNotDecide(t *testing.T) {
	sp := segSizeWorld(t, 3, 64<<20) // a 64 MiB cluster

	keys := segSizeKeys(t, sp)
	if len(keys) != 3 {
		t.Fatalf("fixture: %d keys, want 3", len(keys))
	}

	// The first key the walk would probe is unreadable.
	fi := faultinject.New(sp)
	fi.Activate([]faultinject.Rule{{
		Name:      "first-manifest-unreadable",
		Ops:       faultinject.OpGet,
		KeyPrefix: keys[0],
		Err:       errors.New("simulated 503"),
	}}, faultinject.ActivateOptions{})
	defer fi.Deactivate()

	size, readAny := deploymentSegmentSize(context.Background(), fi, keys)
	if !readAny {
		t.Fatal("readAny=false although two sibling manifests were perfectly readable")
	}
	if size != 64<<20 {
		t.Fatalf("segment size = %d, want %d.\n\nOne unreadable object decided the cluster's "+
			"segment size. Every segment number is derived from it, so `wal audit` would "+
			"report a fabricated gap at every log-id boundary and write wal.gap_detected "+
			"into the audit chain on every run.", size, 64<<20)
	}
}

// When nothing can be read the answer is genuinely unknown, and the
// caller must be told rather than handed the default as if it were a
// fact.
func TestDeploymentSegmentSize_NothingReadableReportsUnknown(t *testing.T) {
	sp := segSizeWorld(t, 2, 64<<20)
	keys := segSizeKeys(t, sp)

	fi := faultinject.New(sp)
	fi.Activate([]faultinject.Rule{{
		Name:      "all-manifests-unreadable",
		Ops:       faultinject.OpGet,
		KeyPrefix: "wal/",
		Err:       errors.New("simulated outage"),
	}}, faultinject.ActivateOptions{})
	defer fi.Deactivate()

	size, readAny := deploymentSegmentSize(context.Background(), fi, keys)
	if readAny {
		t.Fatal("readAny=true although every manifest Get failed")
	}
	if size != walsink.DefaultSegmentSize {
		t.Errorf("fallback size = %d, want the documented default", size)
	}
}

// A manifest that parses but records no segment_size is a LEGACY
// manifest, not an unreadable one: the default is the right answer and
// refusing would break every old repo.
func TestDeploymentSegmentSize_LegacyManifestWithoutSizeIsNotAFailure(t *testing.T) {
	sp := segSizeWorld(t, 1, 0) // valid schema, no segment_size recorded
	keys := segSizeKeys(t, sp)

	size, readAny := deploymentSegmentSize(context.Background(), sp, keys)
	if !readAny {
		t.Fatal("a legacy manifest with no segment_size was treated as unreadable; that " +
			"would refuse every repo written before the field existed")
	}
	if size != walsink.DefaultSegmentSize {
		t.Errorf("size = %d, want the default for a manifest that records none", size)
	}
}

// The probe count is bounded — a repo with a million segments must not
// open a million objects to answer a constant.
func TestDeploymentSegmentSize_ProbeCountIsBounded(t *testing.T) {
	sp := segSizeWorld(t, 2, 64<<20)
	keys := segSizeKeys(t, sp)
	// Pad with keys that do not exist; if the probe were unbounded it
	// would walk all of them.
	for i := 0; i < 5000; i++ {
		keys = append([]string{"wal/db1/00000001/nonexistent.json"}, keys...)
	}
	rec := &countingGetter{StoragePlugin: sp}
	_, _ = deploymentSegmentSize(context.Background(), rec, keys)
	if rec.gets > maxSegmentSizeProbes {
		t.Errorf("issued %d Gets for a cluster-wide constant; the probe is unbounded", rec.gets)
	}
}

// countingGetter counts Get calls, delegating everything else.
type countingGetter struct {
	storage.StoragePlugin
	gets int
}

func (c *countingGetter) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	c.gets++
	return c.StoragePlugin.Get(ctx, key)
}

// segSizeWorld plants n segment manifests for one deployment, each
// recording segmentSize (0 = omit the field, i.e. a legacy manifest).
func segSizeWorld(t *testing.T, n int, segmentSize int64) storage.StoragePlugin {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	nameSize := segmentSize
	if nameSize == 0 {
		nameSize = walsink.DefaultSegmentSize
	}
	for i := 0; i < n; i++ {
		name := walsink.SegmentFileName(1, uint64(i), nameSize)
		body, err := json.Marshal(walsink.SegmentManifest{
			Schema:        walsink.Schema,
			Deployment:    "db1",
			Timeline:      1,
			SegmentNumber: uint64(i),
			SegmentName:   name,
			SegmentSize:   segmentSize,
		})
		if err != nil {
			t.Fatal(err)
		}
		key := walsink.SegmentPath("db1", 1, name)
		if _, err := sp.Put(context.Background(), key, bytes.NewReader(body),
			storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
	}
	return sp
}

// segSizeKeys lists the planted manifest keys in sorted order.
func segSizeKeys(t *testing.T, sp storage.StoragePlugin) []string {
	t.Helper()
	var keys []string
	for info, err := range sp.List(context.Background(), "wal/") {
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(info.Key, ".json") {
			keys = append(keys, info.Key)
		}
	}
	sort.Strings(keys)
	return keys
}
