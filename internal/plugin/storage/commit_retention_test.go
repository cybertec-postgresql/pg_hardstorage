package storage_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// lockModelSP models S3's Object Lock behaviour on the CommitExclusive
// fallback path: a Put carrying RetainUntil locks THAT key, SetRetention
// locks a key, and RenameIfNotExists (CopyObject + DeleteObject on S3)
// carries the bytes but NOT the lock — CopyObject without
// ObjectLockMode/RetainUntilDate produces an unlocked copy. A locked key
// cannot be deleted.
type lockModelSP struct {
	*recordingSP

	mu          sync.Mutex
	locks       map[string]time.Time
	failSetWith error
}

func newLockModelSP(t *testing.T) *lockModelSP {
	r := newRecordingSP(t)
	r.noCondPut = true
	return &lockModelSP{recordingSP: r, locks: map[string]time.Time{}}
}

func (l *lockModelSP) Capabilities() storage.Capabilities {
	c := l.recordingSP.Capabilities()
	c.WORM = true
	return c
}

func (l *lockModelSP) Put(ctx context.Context, key string, r io.Reader, o storage.PutOptions) (storage.PutResult, error) {
	res, err := l.recordingSP.Put(ctx, key, r, o)
	if err == nil && !o.RetainUntil.IsZero() {
		l.mu.Lock()
		l.locks[key] = o.RetainUntil
		l.mu.Unlock()
	}
	return res, err
}

func (l *lockModelSP) SetRetention(_ context.Context, key string, until time.Time, _ storage.WORMMode) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failSetWith != nil {
		return l.failSetWith
	}
	l.locks[key] = until
	return nil
}

func (l *lockModelSP) Delete(ctx context.Context, key string) error {
	l.mu.Lock()
	_, locked := l.locks[key]
	l.mu.Unlock()
	if locked {
		return errors.New("AccessDenied: object is WORM protected")
	}
	return l.recordingSP.Delete(ctx, key)
}

func (l *lockModelSP) RenameIfNotExists(ctx context.Context, src, dst string) error {
	if err := l.recordingSP.StoragePlugin.RenameIfNotExists(ctx, src, dst); err != nil {
		return err
	}
	// S3's rename deletes the source after the copy; a locked source
	// cannot be deleted and stays behind.
	return l.Delete(ctx, src)
}

func (l *lockModelSP) lockOf(key string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.locks[key]
	return t, ok
}

// TestCommitExclusive_FallbackCarriesRetentionToFinalKey pins H13. On a
// backend without conditional PUT (s3 with a custom endpoint, by
// default) the commit staged `<key>.tmp.*` with the lock and renamed it;
// the copy dropped the lock, so the committed WAL/timeline manifest was
// UNLOCKED while an undeletable locked temporary was left behind.
func TestCommitExclusive_FallbackCarriesRetentionToFinalKey(t *testing.T) {
	sp := newLockModelSP(t)
	until := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)

	err := storage.CommitExclusive(context.Background(), sp, "wal/000000010000000000000001.json",
		[]byte("manifest"), storage.PutOptions{RetainUntil: until, RetentionMode: storage.WORMCompliance})
	if err != nil {
		t.Fatalf("CommitExclusive: %v", err)
	}
	got, ok := sp.lockOf("wal/000000010000000000000001.json")
	if !ok {
		t.Fatal("committed object carries no retention: the lock stayed on the staging object")
	}
	if !got.Equal(until) {
		t.Errorf("retain-until = %v, want %v", got, until)
	}
	for _, e := range listAll(t, sp, "wal/") {
		if strings.Contains(e, ".tmp.") {
			t.Errorf("staging object %q left behind (locked, so the rename could not remove it)", e)
		}
	}
}

// TestCommitExclusive_FallbackFailsWhenRetentionCannotBeApplied: an
// unlocked manifest must not be reported as committed.
func TestCommitExclusive_FallbackFailsWhenRetentionCannotBeApplied(t *testing.T) {
	sp := newLockModelSP(t)
	sp.failSetWith = errors.New("InvalidRequest: bucket has no Object Lock configuration")

	err := storage.CommitExclusive(context.Background(), sp, "m/a.json", []byte("x"),
		storage.PutOptions{RetainUntil: time.Now().Add(time.Hour)})
	if err == nil {
		t.Fatal("CommitExclusive reported success although the committed object could not be locked")
	}
	if _, serr := sp.Stat(context.Background(), "m/a.json"); !errors.Is(serr, storage.ErrNotFound) {
		t.Errorf("unlocked object left at the key after a failed lock (Stat err = %v)", serr)
	}
}

func listAll(t *testing.T, sp storage.StoragePlugin, prefix string) []string {
	t.Helper()
	var out []string
	for info, err := range sp.List(context.Background(), prefix) {
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		out = append(out, info.Key)
	}
	return out
}
