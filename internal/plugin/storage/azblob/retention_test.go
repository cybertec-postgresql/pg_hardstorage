package azblob_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// TestAzblob_PutWithRetentionFailsClosed pins the fix. The CAS's chunk path
// carries the WORM deadline in PutOptions.RetainUntil and relies on Put to
// enforce it. Before the fix azblob Put ignored RetainUntil, silently
// writing WORM-configured chunks deletable. Azurite doesn't implement the
// immutability-policy API, so it's the perfect probe: with the fix, a Put
// that cannot apply the requested lock must FAIL CLOSED — error AND roll
// back — so no unlocked object is left looking like a committed, protected
// one. (Before the fix this Put "succeeded" and left an unprotected blob.)
func TestAzblob_PutWithRetentionFailsClosed(t *testing.T) {
	p := openAzblobOnFreshAzurite(t)
	ctx := context.Background()
	key := "worm/chunk-immutable"
	body := "compliance-locked bytes"

	_, err := p.Put(ctx, key, strings.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
		RetainUntil:   time.Now().Add(2 * time.Hour),
		RetentionMode: storage.WORMCompliance,
	})
	if err == nil {
		t.Fatalf("Put with RetainUntil succeeded against a backend with no immutability API — RetainUntil was ignored (the bug)")
	}
	if _, serr := p.Stat(ctx, key); serr == nil {
		t.Errorf("an unlockable object must be rolled back; it is still present after a failed-retention Put")
	} else if !errors.Is(serr, storage.ErrNotFound) {
		t.Errorf("Stat after rollback: unexpected error %v", serr)
	}
}
