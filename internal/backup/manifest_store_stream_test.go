package backup_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

func TestStreamFiles_MatchesRead(t *testing.T) {
	store, _, signer, verifier := newStore(t)
	m := validManifest()
	ctx := context.Background()
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	want, err := store.Read(ctx, m.Deployment, m.BackupID, verifier)
	if err != nil {
		t.Fatal(err)
	}
	var got []backup.FileEntry
	info, err := store.StreamFiles(ctx, m.Deployment, m.BackupID, verifier, func(fe *backup.FileEntry) error {
		got = append(got, *fe)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want.Files) {
		t.Errorf("entries differ:\n got %+v\nwant %+v", got, want.Files)
	}
	if !info.Verified || info.Files != int64(len(want.Files)) || info.Manifest.Files != nil {
		t.Errorf("info = %+v", info)
	}
	// Summary read: no callback.
	sum, err := store.StreamFiles(ctx, m.Deployment, m.BackupID, verifier, nil)
	if err != nil || sum.Files != info.Files || sum.ChunkRefs != info.ChunkRefs || sum.Bytes != info.Bytes {
		t.Errorf("summary = %+v err=%v, want %+v", sum, err, info)
	}
	if _, err := store.StreamFiles(ctx, m.Deployment, m.BackupID, nil, nil); err == nil {
		t.Error("nil verifier accepted")
	}
}

func TestStreamFiles_TombstoneAndMissing(t *testing.T) {
	store, _, signer, verifier := newStore(t)
	ctx := context.Background()
	m := validManifest()
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StreamFiles(ctx, "db1", "db1.full.nope", verifier, nil); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if err := store.SoftDelete(ctx, m.Deployment, m.BackupID, "gfs", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StreamFiles(ctx, m.Deployment, m.BackupID, verifier, nil); !errors.Is(err, backup.ErrTombstoned) {
		t.Errorf("tombstoned: %v", err)
	}
}

func TestStreamFiles_FallsBackToReplicaOnCorruptPrimary(t *testing.T) {
	store, sp, signer, verifier := newStore(t)
	ctx := context.Background()
	m := validManifest()
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	primary := backup.PrimaryPath(m.Deployment, m.BackupID)
	if err := sp.Delete(ctx, primary); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"schema":"pg_hardstorage.manifest.v1","garbage":`)
	if _, err := sp.Put(ctx, primary, bytesReader(body), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	n := 0
	if _, err := store.StreamFiles(ctx, m.Deployment, m.BackupID, verifier, func(*backup.FileEntry) error { n++; return nil }); err != nil {
		t.Fatalf("replica fallback: %v", err)
	}
	if n != len(m.Files) {
		t.Errorf("delivered %d entries, want %d", n, len(m.Files))
	}
}

func TestStreamFiles_CallbackErrorIsFinalAndNotRetriedOnReplica(t *testing.T) {
	store, _, signer, verifier := newStore(t)
	ctx := context.Background()
	m := validManifest()
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	calls := 0
	_, err := store.StreamFiles(ctx, m.Deployment, m.BackupID, verifier, func(*backup.FileEntry) error { calls++; return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if calls != 1 {
		t.Errorf("callback ran %d times; the replica must not replay entries", calls)
	}
	calls = 0
	_, err = store.StreamFilesAttestationless(ctx, m.Deployment, m.BackupID, func(*backup.FileEntry) error { calls++; return boom })
	if !errors.Is(err, boom) || calls != 1 {
		t.Errorf("attestationless: err=%v calls=%d", err, calls)
	}
}

func TestStreamFiles_NothingDeliveredFromTamperedManifest(t *testing.T) {
	store, sp, signer, verifier := newStore(t)
	ctx := context.Background()
	m := validManifest()
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{backup.PrimaryPath(m.Deployment, m.BackupID), backup.ReplicaPath(m.BackupID)} {
		rc, err := sp.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		raw := readAll(t, rc)
		raw = bytesReplace(raw, `"size":100`, `"size":101`)
		_ = sp.Delete(ctx, key)
		if _, err := sp.Put(ctx, key, bytesReader(raw), storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	_, err := store.StreamFiles(ctx, m.Deployment, m.BackupID, verifier, func(*backup.FileEntry) error { n++; return nil })
	if err == nil {
		t.Fatal("tampered manifest accepted")
	}
	if n != 0 {
		t.Errorf("%d entries delivered from a manifest whose signature does not verify", n)
	}
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func readAll(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func bytesReplace(b []byte, old, new string) []byte {
	out := bytes.Replace(b, []byte(old), []byte(new), 1)
	if bytes.Equal(out, b) {
		panic("bytesReplace: no change")
	}
	return out
}

func TestReadStreaming_EachFileMatchesHeldManifest(t *testing.T) {
	store, _, signer, verifier := newStore(t)
	ctx := context.Background()
	m := validManifest()
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	held, err := store.Read(ctx, m.Deployment, m.BackupID, verifier)
	if err != nil {
		t.Fatal(err)
	}
	sm, err := store.ReadStreaming(ctx, m.Deployment, m.BackupID, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if !sm.Streaming() || sm.Files != nil || held.Streaming() {
		t.Fatalf("streaming flags wrong: streamed=%v files=%v held=%v", sm.Streaming(), sm.Files, held.Streaming())
	}
	if sm.NumFiles() != held.NumFiles() || sm.TotalFileBytes() != held.TotalFileBytes() || sm.NumChunkRefs() != held.NumChunkRefs() {
		t.Errorf("counts differ: streamed %d/%d/%d held %d/%d/%d",
			sm.NumFiles(), sm.TotalFileBytes(), sm.NumChunkRefs(), held.NumFiles(), held.TotalFileBytes(), held.NumChunkRefs())
	}
	var a, b []backup.FileEntry
	if err := sm.EachFile(ctx, func(fe *backup.FileEntry) error { a = append(a, *fe); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := held.EachFile(ctx, func(fe *backup.FileEntry) error { b = append(b, *fe); return nil }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("EachFile differs:\n streamed %+v\n held     %+v", a, b)
	}
	if err := sm.Validate(); err != nil {
		t.Errorf("streamed Validate: %v", err)
	}
	// A manifest without its file list must refuse to be re-signed or re-marshalled.
	if _, err := sm.Canonicalize(); !errors.Is(err, backup.ErrStreamedManifest) {
		t.Errorf("Canonicalize: %v", err)
	}
	if _, err := sm.MarshalToBytes(); !errors.Is(err, backup.ErrStreamedManifest) {
		t.Errorf("MarshalToBytes: %v", err)
	}
	if err := sm.Sign(signer); !errors.Is(err, backup.ErrStreamedManifest) {
		t.Errorf("Sign: %v", err)
	}
	// Callback error is returned as-is and stops the walk.
	boom := errors.New("boom")
	n := 0
	if err := sm.EachFile(ctx, func(*backup.FileEntry) error { n++; return boom }); !errors.Is(err, boom) || n != 1 {
		t.Errorf("callback error: err=%v calls=%d", err, n)
	}
}

func TestReadStreaming_DetectsSwapAfterVerification(t *testing.T) {
	store, sp, signer, verifier := newStore(t)
	ctx := context.Background()
	m := validManifest()
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	sm, err := store.ReadStreaming(ctx, m.Deployment, m.BackupID, verifier)
	if err != nil {
		t.Fatal(err)
	}
	key := backup.PrimaryPath(m.Deployment, m.BackupID)
	rc, _ := sp.Get(ctx, key)
	raw := bytesReplace(readAll(t, rc), `"size":100`, `"size":101`)
	_ = sp.Delete(ctx, key)
	if _, err := sp.Put(ctx, key, bytesReader(raw), storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
		t.Fatal(err)
	}
	if err := sm.EachFile(ctx, func(*backup.FileEntry) error { return nil }); err == nil {
		t.Fatal("EachFile did not notice the object changed after verification")
	}
}

// TestListHeaders_YieldsHeaderOnlyManifests proves the retention listing
// holds no file lists even for a small manifest (List and ListAuto keep
// those whole), while still carrying the header fields and file counts.
func TestListHeaders_YieldsHeaderOnlyManifests(t *testing.T) {
	store, _, signer, verifier := newStore(t)
	m := validManifest()
	ctx := context.Background()
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	n := 0
	for got, err := range store.ListHeaders(ctx, m.Deployment, verifier) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		if got.Files != nil || !got.Streaming() {
			t.Errorf("ListHeaders held the file list (Files=%v streaming=%v)", got.Files != nil, got.Streaming())
		}
		if got.BackupID != m.BackupID || got.NumFiles() != len(m.Files) {
			t.Errorf("header = %s files=%d, want %s files=%d", got.BackupID, got.NumFiles(), m.BackupID, len(m.Files))
		}
	}
	if n != 1 {
		t.Fatalf("yielded %d manifests, want 1", n)
	}
	for _, err := range store.ListHeaders(ctx, m.Deployment, nil) {
		if err == nil {
			t.Error("nil verifier accepted")
		}
		break
	}
}
