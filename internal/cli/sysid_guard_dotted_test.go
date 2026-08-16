package cli

import (
	"bytes"
	"context"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// guardSystemIdentifier must still refuse a foreign cluster's WAL for a
// deployment whose NAME contains ".json.tmp." — else the anti-corruption
// guard (#7) is silently disabled and cluster B's WAL interleaves with
// cluster A's lineage (repo corruption / PITR data loss).
func TestGuardSystemIdentifier_DottedDeployment(t *testing.T) {
	ctx := context.Background()
	sp := &fs.Plugin{}
	if err := sp.Open(ctx, storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	dep := "db.json.tmp.x"
	seg := "wal/" + dep + "/00000001/000000010000000000000001.json"
	body := `{"schema":"pg_hardstorage.wal_segment.v1","start_lsn":"0/1000000","end_lsn":"0/2000000","segment_size":16777216,"system_identifier":"1111111111111111111"}`
	if _, err := sp.Put(ctx, seg, bytes.NewReader([]byte(body)), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}

	// Live cluster has a DIFFERENT system_identifier → must refuse.
	err := guardSystemIdentifier(ctx, sp, "wal stream", dep, "2222222222222222222", false)
	if err == nil {
		t.Fatalf("guardSystemIdentifier ALLOWED a changed system_identifier for deployment %q — the "+
			"foreign-cluster guard is disabled (all segments skipped as 'tmp'), so cluster B's WAL would "+
			"corrupt cluster A's lineage (DATA LOSS)", dep)
	}
}
