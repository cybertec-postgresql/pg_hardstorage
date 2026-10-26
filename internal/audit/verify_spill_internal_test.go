package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// verifyShard keeps linkage tuples in an external sorter. Forced to spill
// after a KiB, it must still pass a good chain and pinpoint a broken link.
func TestVerifyChain_LinkageSurvivesSpill(t *testing.T) {
	old := verifySortMemBytes
	verifySortMemBytes = 1 << 10
	defer func() { verifySortMemBytes = old }()

	root := t.TempDir()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	store := NewStore(sp)
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(shardEventDir(""))), 0o755); err != nil {
		t.Fatal(err)
	}
	const n = 1500
	prev := GenesisHash
	var last *Event
	write := func(ev *Event) {
		body, _ := json.Marshal(ev)
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(keyFor(ev))), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var evs []*Event
	for i := 0; i < n; i++ {
		ev := &Event{Schema: Schema, Action: "a", Actor: "x", Timestamp: time.Unix(1_700_000_000+int64(i), 0).UTC(),
			ID: fmt.Sprintf("ev-%05d", i), Sequence: int64(i), PrevHash: prev}
		h, err := ComputeHash(ev)
		if err != nil {
			t.Fatal(err)
		}
		ev.Hash, prev = h, h
		write(ev)
		evs = append(evs, ev)
		last = ev
	}
	if err := store.writeHeadPointer(context.Background(), "", last, keyFor(last)); err != nil {
		t.Fatal(err)
	}
	res, err := store.VerifyChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.EventsChecked != n {
		t.Fatalf("good chain: %+v", res)
	}
	// Re-link event 700 onto a wrong predecessor (re-hashing so its own
	// hash stays self-consistent): only the link is broken.
	bad := *evs[700]
	bad.PrevHash = evs[10].Hash
	h, _ := ComputeHash(&bad)
	bad.Hash = h
	write(&bad)
	res, err = store.VerifyChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 700's link and 701's (its predecessor's hash changed) both break.
	if res.OK || len(res.ChainBreaks) != 2 || len(res.HashMismatches) != 0 {
		t.Fatalf("broken chain: %+v", res)
	}
}
