package backup_test

import (
	"context"
	"crypto/rand"
	"io"
	"iter"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// recordingStorage wraps storage.StoragePlugin and captures every
// PutOptions for inspection (mirror of the repo-package version).
type recordingStorage struct {
	storage.NopBarrier // test fake: Inline-only, no deferred writes
	mu                 sync.Mutex
	inner              storage.StoragePlugin
	puts               []recordedPut
	retentions         []recordedRetention
}

type recordedPut struct {
	Key  string
	Opts storage.PutOptions
}

type recordedRetention struct {
	Key   string
	Until time.Time
	Mode  storage.WORMMode
}

func (r *recordingStorage) Name() string { return r.inner.Name() }
func (r *recordingStorage) Open(ctx context.Context, cfg storage.StorageConfig) error {
	return r.inner.Open(ctx, cfg)
}
func (r *recordingStorage) Put(ctx context.Context, key string, src io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	r.mu.Lock()
	r.puts = append(r.puts, recordedPut{Key: key, Opts: opts})
	r.mu.Unlock()
	return r.inner.Put(ctx, key, src, opts)
}
func (r *recordingStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return r.inner.Get(ctx, key)
}
func (r *recordingStorage) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	return r.inner.Stat(ctx, key)
}
func (r *recordingStorage) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	return r.inner.List(ctx, prefix)
}
func (r *recordingStorage) Delete(ctx context.Context, key string) error {
	return r.inner.Delete(ctx, key)
}
func (r *recordingStorage) RenameIfNotExists(ctx context.Context, src, dst string) error {
	return r.inner.RenameIfNotExists(ctx, src, dst)
}
func (r *recordingStorage) SetRetention(ctx context.Context, key string, until time.Time, mode storage.WORMMode) error {
	r.mu.Lock()
	r.retentions = append(r.retentions, recordedRetention{Key: key, Until: until, Mode: mode})
	r.mu.Unlock()
	return r.inner.SetRetention(ctx, key, until, mode)
}
func (r *recordingStorage) Capabilities() storage.Capabilities { return r.inner.Capabilities() }
func (r *recordingStorage) Close() error                       { return r.inner.Close() }

// TestManifestStore_Commit_PropagatesRetention: CommitOptions.
// RetainUntil + RetentionMode are applied to the COMMITTED manifest
// objects (primary + replica) via SetRetention — not to the staging
// tmp Put.  Locking the tmp instead was wrong: on a Compliance bucket
// the rename's source-delete can't remove a locked tmp, and the copy
// doesn't carry the lock to the committed object anyway.
func TestManifestStore_Commit_PropagatesRetention(t *testing.T) {
	root := t.TempDir()
	inner := &fs.Plugin{}
	if err := inner.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	rec := &recordingStorage{inner: inner}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	_, _ = backup.LoadVerifier(pub)

	store := backup.NewManifestStore(rec)
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.worm",
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces: []backup.Tablespace{
			{OID: 1663, Location: "pg_default"},
		},
	}
	until := time.Date(2033, 4, 30, 12, 0, 0, 0, time.UTC)
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{
		RetainUntil:   until,
		RetentionMode: storage.WORMCompliance,
	}); err != nil {
		t.Fatal(err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()

	// The staging Puts must NOT carry retention — a locked tmp breaks
	// the rename's source-delete on a Compliance bucket.
	for _, p := range rec.puts {
		if !p.Opts.RetainUntil.IsZero() || p.Opts.RetentionMode != "" {
			t.Errorf("Put %q carried retention (RetainUntil=%s mode=%q); it must go to the committed object instead",
				p.Key, p.Opts.RetainUntil, p.Opts.RetentionMode)
		}
	}

	// SetRetention must have been applied to the committed primary and
	// replica manifests (never to a .tmp. staging key), each with the
	// requested deadline + compliance mode.
	var primary, replica bool
	for _, r := range rec.retentions {
		if strings.Contains(r.Key, ".tmp.") {
			t.Errorf("retention applied to a staging key %q", r.Key)
		}
		if !r.Until.Equal(until) {
			t.Errorf("SetRetention %q: until = %s, want %s", r.Key, r.Until, until)
		}
		if r.Mode != storage.WORMCompliance {
			t.Errorf("SetRetention %q: mode = %q, want compliance", r.Key, r.Mode)
		}
		switch {
		case strings.HasPrefix(r.Key, "manifests/_replicas/"):
			replica = true
		case strings.HasSuffix(r.Key, "/manifest.json"):
			primary = true
		}
	}
	if !primary {
		t.Error("SetRetention was not applied to the committed primary manifest")
	}
	if !replica {
		t.Error("SetRetention was not applied to the committed replica manifest")
	}
}

// TestManifestStore_Commit_NoRetention_NoPropagation: a Commit
// without retention options leaves the Put's RetainUntil zero.
func TestManifestStore_Commit_NoRetention_NoPropagation(t *testing.T) {
	root := t.TempDir()
	inner := &fs.Plugin{}
	inner.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	})
	defer inner.Close()
	rec := &recordingStorage{inner: inner}

	priv, _, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)

	store := backup.NewManifestStore(rec)
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         "db1.full.no-worm",
		Deployment:       "db1",
		Tenant:           "default",
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces: []backup.Tablespace{
			{OID: 1663, Location: "pg_default"},
		},
	}
	if err := store.Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, p := range rec.puts {
		if !p.Opts.RetainUntil.IsZero() {
			t.Errorf("Put %q: RetainUntil = %s, want zero", p.Key, p.Opts.RetainUntil)
		}
	}
	// And SetRetention must not be called when no retention was asked.
	if len(rec.retentions) != 0 {
		t.Errorf("expected no SetRetention calls; got %d", len(rec.retentions))
	}
}
