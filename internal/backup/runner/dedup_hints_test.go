package runner

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestLoadDedupHints_FilterSizeIsCapped proves the hint filter does not grow
// with the prior manifest: with the cap lowered, a manifest of many chunks
// still loads (every hash is reported as counted) but the filter is sized for
// the cap, not for the chunk count.
func TestLoadDedupHints_FilterSizeIsCapped(t *testing.T) {
	old := maxHintFilterKeys
	maxHintFilterKeys = 8
	t.Cleanup(func() { maxHintFilterKeys = old })

	sp := adoptTestRepo(t)
	var chunks bytes.Buffer
	hashes := make([]repo.Hash, 200)
	for i := range hashes {
		hashes[i] = repo.HashOf([]byte(fmt.Sprintf("chunk-%d", i)))
		if i > 0 {
			chunks.WriteByte(',')
		}
		fmt.Fprintf(&chunks, `{"hash":%q,"offset":%d,"len":1}`, hashes[i].String(), i)
	}
	doc := fmt.Sprintf(`{"schema":"pg_hardstorage.manifest.v1","backup_id":"b","deployment":"dep","files":[{"path":"f","size":200,"chunks":[%s]}]}`, chunks.String())
	key := "manifests/dep/backups/b/manifest.json"
	if _, err := sp.Put(context.Background(), key, bytes.NewReader([]byte(doc)), storage.PutOptions{ContentLength: int64(len(doc))}); err != nil {
		t.Fatal(err)
	}

	f, n, err := loadDedupHints(context.Background(), sp, "dep")
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(hashes)) {
		t.Fatalf("counted %d hashes, want %d", n, len(hashes))
	}
	// No false negatives, even saturated.
	for _, h := range hashes {
		if !f.Test(h) {
			t.Fatalf("hash %s missing from the filter", h)
		}
	}
}
