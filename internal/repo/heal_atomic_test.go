package repo_test

// heal_atomic_test.go — the two heal fixes from the corruption audit.
//
// H-1: heal used delete-then-Put(IfNotExists). The conditional put
// FORCED the delete (it would have no-op'd against the corrupt
// object), and the delete opened a crash window in which the chunk
// existed nowhere locally: an interrupted heal turned "corrupt" into
// "absent", strictly worse than what it found. It also made heal fail
// outright on WORM repos, where deleting a retention-locked chunk is
// refused. The replacement is ONE atomic overwrite Put — no delete
// ever happens on the heal path, which is asserted structurally here.
//
// H-2: heal copied replica bytes with no validation at all, so a
// replica whose copy was truncated or rotted past parseability
// "healed" the local chunk with provably-broken bytes and reported
// success. Plaintext verification needs the DEK (that is
// reverifyChunksPlaintext's job, one layer up) — but an envelope that
// does not even PARSE is refusable keylessly, and now is.

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// deleteCounter fails the invariant loudly if anything deletes.
type deleteCounter struct {
	storage.StoragePlugin
	deletes atomic.Int64
}

func (d *deleteCounter) Delete(ctx context.Context, key string) error {
	d.deletes.Add(1)
	return d.StoragePlugin.Delete(ctx, key)
}

func TestHeal_NeverDeletes(t *testing.T) {
	dst, replica := twoRepos(t)
	body := []byte("healable")
	h := putChunk(t, replica, env(body))
	corruptChunkAt(t, dst, h, env([]byte("locally-rotted")))

	counted := &deleteCounter{StoragePlugin: dst}
	res, err := repo.Heal(context.Background(), counted, replica, []repo.Hash{h}, repo.HealOptions{})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if res.Healed != 1 {
		t.Fatalf("Healed=%d, want 1: %+v", res.Healed, res)
	}
	if got := readChunkBytes(t, dst, h); !bytes.Equal(got, env(body)) {
		t.Fatalf("post-heal bytes wrong: %q", got)
	}
	if n := counted.deletes.Load(); n != 0 {
		t.Fatalf("heal performed %d Delete(s) — the delete-then-put shape is back, and with "+
			"it the crash window where a corrupt chunk becomes an ABSENT one (and the "+
			"guaranteed failure on WORM repos, where the delete is refused)", n)
	}
}

func TestHeal_RefusesAnUnparseableReplicaCopy(t *testing.T) {
	dst, replica := twoRepos(t)
	body := []byte("the original")
	h := putChunk(t, replica, env(body))
	localBytes := env([]byte("local-rot"))
	corruptChunkAt(t, dst, h, localBytes)
	// The replica's copy is rotted PAST parseability — not an envelope
	// at all. Healing FROM it would install provably-broken bytes.
	corruptChunkAt(t, replica, h, []byte("\xffnot an envelope"))

	res, err := repo.Heal(context.Background(), dst, replica, []repo.Hash{h}, repo.HealOptions{})
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if res.Failed != 1 || res.Healed != 0 {
		t.Fatalf("Failed=%d Healed=%d, want 1/0 — an unparseable replica copy must refuse, "+
			"not \"heal\": %+v", res.Failed, res.Healed, res)
	}
	if len(res.Failures) == 0 || !strings.Contains(res.Failures[0].Err, "replica") {
		t.Errorf("the failure must point at the REPLICA side so the operator repairs the "+
			"right copy: %+v", res.Failures)
	}
	// The local copy — bad as it is — must be untouched: heal must
	// never leave LESS than it found.
	if got := readChunkBytes(t, dst, h); !bytes.Equal(got, localBytes) {
		t.Errorf("local copy modified despite the refusal: %q", got)
	}
}
