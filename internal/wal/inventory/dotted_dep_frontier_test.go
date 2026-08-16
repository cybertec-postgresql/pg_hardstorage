package inventory_test

// dotted_dep_frontier_test.go — the archive frontier must not go blind for a
// deployment whose name contains ".json.tmp." (validateStorageID permits
// dots). Regression for mutation_inventory_temp_fullkey: a full-key temp skip
// dropped every segment of such a deployment, so HighestArchivedLSN /
// NextArchivedLSNAtOrAfter reported nothing archived — silently missing
// failover gaps and mis-bounding restore.

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/inventory"
)

func TestFrontier_UnderDottedDeployment(t *testing.T) {
	ctx := context.Background()
	sp := &fs.Plugin{}
	if err := sp.Open(ctx, storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	dep := "db.json.tmp.x"
	key := fmt.Sprintf("wal/%s/00000001/000000010000000000000001.json", dep)
	body := `{"schema":"pg_hardstorage.wal_segment.v1","start_lsn":"0/1000000","end_lsn":"0/2000000","segment_size":16777216}`
	if _, err := sp.Put(ctx, key, bytes.NewReader([]byte(body)), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}

	lsn, found, err := inventory.HighestArchivedLSN(ctx, sp, dep, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("HighestArchivedLSN blind for deployment %q despite a committed segment — failover gap "+
			"silently missed / restore bounds wrong (DATA LOSS)", dep)
	}
	if want, _ := pglogrepl.ParseLSN("0/2000000"); lsn != want {
		t.Fatalf("HighestArchivedLSN = %s, want %s", lsn, want)
	}

	// NextArchivedLSNAtOrAfter must also see the segment.
	nlsn, nfound, err := inventory.NextArchivedLSNAtOrAfter(ctx, sp, dep, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !nfound {
		t.Fatalf("NextArchivedLSNAtOrAfter blind for deployment %q despite a committed segment (DATA LOSS)", dep)
	}
	_ = nlsn
}
