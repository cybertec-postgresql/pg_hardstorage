package storage_test

// commit_test.go — CommitExclusive must publish exclusively, and on a
// capable backend must do it without deleting anything.
//
// Issue #45: manifests were published as `<key>.tmp.<rand>` +
// RenameIfNotExists, which on S3 is Head + Copy(IfNoneMatch) + DELETE.
// A repository serving as an anti-ransomware copy of record then
// accumulated a delete marker per WAL segment, and a store without a
// conditional COPY could not commit a manifest at all — while
// implementing conditional PUT perfectly well.
//
// The property that matters is not "uses a PUT"; it is "the object
// appears whole or not at all, a second writer loses, and nothing is
// deleted when the backend can manage it". These test that directly.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// recordingSP counts the operations a commit performs.
type recordingSP struct {
	storage.StoragePlugin

	mu           sync.Mutex
	deletes      []string
	renames      int
	noCondPut    bool
	failCopyWith error // what RenameIfNotExists returns, if set
}

func (r *recordingSP) Capabilities() storage.Capabilities {
	c := r.StoragePlugin.Capabilities()
	if r.noCondPut {
		c.ConditionalPut = false
	}
	return c
}

func (r *recordingSP) Delete(ctx context.Context, key string) error {
	r.mu.Lock()
	r.deletes = append(r.deletes, key)
	r.mu.Unlock()
	return r.StoragePlugin.Delete(ctx, key)
}

func (r *recordingSP) RenameIfNotExists(ctx context.Context, src, dst string) error {
	r.mu.Lock()
	r.renames++
	fail := r.failCopyWith
	r.mu.Unlock()
	if fail != nil {
		return fail
	}
	return r.StoragePlugin.RenameIfNotExists(ctx, src, dst)
}

func (r *recordingSP) Deletes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.deletes...)
}

func (r *recordingSP) Renames() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.renames
}

func newRecordingSP(t *testing.T) *recordingSP {
	t.Helper()
	p := &fs.Plugin{}
	u, err := url.Parse("file://" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatalf("open fs: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return &recordingSP{StoragePlugin: p}
}

// TestCommitExclusive_ConditionalPutDeletesNothing is the append-only
// property, and the reason the issue was filed.
func TestCommitExclusive_ConditionalPutDeletesNothing(t *testing.T) {
	sp := newRecordingSP(t)
	if !sp.Capabilities().ConditionalPut {
		t.Fatal("fixture does not advertise ConditionalPut; this test would prove nothing")
	}

	body := []byte(`{"schema":"manifest"}`)
	if err := storage.CommitExclusive(context.Background(), sp, "wal/db1/seg.json", body,
		storage.PutOptions{}); err != nil {
		t.Fatalf("CommitExclusive: %v", err)
	}

	if d := sp.Deletes(); len(d) != 0 {
		t.Errorf("commit issued %d delete(s): %v\nOn a versioned bucket each one is a "+
			"delete marker, and a repository kept as an anti-ransomware copy of record "+
			"treats that as an anomaly — which is why WAL archiving could not be adopted "+
			"there at all", len(d), d)
	}
	if n := sp.Renames(); n != 0 {
		t.Errorf("commit used %d rename(s); a backend with conditional PUT needs no "+
			"staging object, and the rename is what requires a conditional COPY that "+
			"many S3-compatible stores do not implement", n)
	}
}

// TestCommitExclusive_RejectsSecondWriter is the exclusion half. The
// staging shape existed to get this; dropping it must not drop that.
func TestCommitExclusive_RejectsSecondWriter(t *testing.T) {
	for _, tc := range []struct {
		name      string
		noCondPut bool
	}{
		{"conditional put", false},
		{"staging fallback", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := newRecordingSP(t)
			sp.noCondPut = tc.noCondPut
			ctx := context.Background()
			key := "wal/db1/seg.json"

			if err := storage.CommitExclusive(ctx, sp, key, []byte("first"),
				storage.PutOptions{}); err != nil {
				t.Fatalf("first commit: %v", err)
			}
			err := storage.CommitExclusive(ctx, sp, key, []byte("second"),
				storage.PutOptions{})
			if !errors.Is(err, storage.ErrAlreadyExists) {
				t.Fatalf("second commit err = %v, want ErrAlreadyExists.\nA manifest key "+
					"that can be overwritten means two backups racing one ID silently "+
					"clobber each other", err)
			}

			// The loser must not have changed the stored body.
			rc, gerr := sp.Get(ctx, key)
			if gerr != nil {
				t.Fatalf("Get: %v", gerr)
			}
			defer rc.Close()
			buf := make([]byte, 16)
			n, _ := rc.Read(buf)
			if got := string(buf[:n]); !strings.HasPrefix(got, "first") {
				t.Errorf("stored body = %q; the rejected writer overwrote the winner", got)
			}
		})
	}
}

// TestCommitExclusive_FallbackStillWorksWithoutConditionalPut pins that
// backends which cannot do a conditional PUT keep working. sftp against
// a server without hardlink@openssh.com is the live case; removing the
// staging path outright would break it.
func TestCommitExclusive_FallbackStillWorksWithoutConditionalPut(t *testing.T) {
	sp := newRecordingSP(t)
	sp.noCondPut = true

	if err := storage.CommitExclusive(context.Background(), sp, "m/x.json",
		[]byte("body"), storage.PutOptions{}); err != nil {
		t.Fatalf("CommitExclusive on a backend without ConditionalPut: %v", err)
	}
	if sp.Renames() == 0 {
		t.Error("the fallback did not use RenameIfNotExists; without conditional PUT that " +
			"is the only way to get exclusion, so silently doing a plain overwrite would " +
			"lose it")
	}
}

// TestCommitExclusive_FallbackCleansUpItsStagingObject pins that a
// failed rename does not strand the temporary. It is the one delete the
// fallback legitimately performs, and only on the failure path.
func TestCommitExclusive_FallbackCleansUpItsStagingObject(t *testing.T) {
	sp := newRecordingSP(t)
	sp.noCondPut = true
	sp.failCopyWith = fmt.Errorf("s3: copy: %w", storage.ErrUnsupported)

	err := storage.CommitExclusive(context.Background(), sp, "m/y.json",
		[]byte("body"), storage.PutOptions{})
	if !errors.Is(err, storage.ErrUnsupported) {
		t.Fatalf("err = %v, want the rename's own error to surface", err)
	}
	d := sp.Deletes()
	if len(d) != 1 || !strings.Contains(d[0], "m/y.json.tmp.") {
		t.Errorf("expected exactly one delete of the staging object, got %v", d)
	}
}

// TestCommitExclusive_StagingNamesAreUnique guards the fallback against
// two concurrent committers sharing a temporary, which would let one
// publish the other's bytes.
func TestCommitExclusive_StagingNamesAreUnique(t *testing.T) {
	sp := newRecordingSP(t)
	sp.noCondPut = true
	sp.failCopyWith = errors.New("forced")

	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		_ = storage.CommitExclusive(context.Background(), sp, "m/z.json",
			[]byte("body"), storage.PutOptions{})
	}
	for _, d := range sp.Deletes() {
		if seen[d] {
			t.Fatalf("staging key %q reused; two concurrent commits would share one "+
				"temporary and could publish each other's bytes", d)
		}
		seen[d] = true
	}
}
