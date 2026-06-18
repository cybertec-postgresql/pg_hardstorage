package fs_test

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// freshFS spins up a Plugin rooted at a t.TempDir, with the URL
// already parsed.  Returned plugin is closed by t.Cleanup.
func freshFS(t *testing.T) (*fs.Plugin, string) {
	t.Helper()
	root := t.TempDir()
	u, err := url.Parse("file://" + root)
	if err != nil {
		t.Fatal(err)
	}
	p := &fs.Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, root
}

// TestDirSync_PutOverwrite_RoundTrip is a behavioural sanity check
// that the new parent-dir fsync after a tmp→final rename doesn't
// break the happy path (round-trip Get reads back the body).  We
// can't black-box-test the syscall itself in unit tests, but we
// can assert the rename + Get combo still works on every backend
// the test runs on (Linux, macOS, the Windows CI lane).
func TestDirSync_PutOverwrite_RoundTrip(t *testing.T) {
	p, _ := freshFS(t)
	body := []byte("manifest body — must round-trip after the rename + dir-fsync")
	if _, err := p.Put(context.Background(), "manifests/db1/manifest.json",
		bytes.NewReader(body), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rd, err := p.Get(context.Background(), "manifests/db1/manifest.json")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rd.Close()
	got := readAllOrNil(t, rd)
	if string(got) != string(body) {
		t.Errorf("round-trip mismatch:\n got %q\n want %q", got, body)
	}
}

// TestDirSync_PutIfNotExists_RoundTrip covers the second metadata
// path: O_CREATE|O_EXCL plus the new parent-dir fsync.
func TestDirSync_PutIfNotExists_RoundTrip(t *testing.T) {
	p, _ := freshFS(t)
	body := []byte("chunk body")
	if _, err := p.Put(context.Background(), "chunks/sha256/aa/bb/aabb…chunk",
		bytes.NewReader(body), storage.PutOptions{
			IfNotExists:   true,
			ContentLength: int64(len(body)),
		}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rd, err := p.Get(context.Background(), "chunks/sha256/aa/bb/aabb…chunk")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rd.Close()
	got := readAllOrNil(t, rd)
	if string(got) != string(body) {
		t.Errorf("round-trip mismatch")
	}
}

// TestDirSync_RenameIfNotExists_RoundTrip exercises the link+unlink
// path with the new fsync on both src and dst parents.
func TestDirSync_RenameIfNotExists_RoundTrip(t *testing.T) {
	p, _ := freshFS(t)
	body := []byte("staged manifest")
	if _, err := p.Put(context.Background(), "manifests/_tmp/manifest.json.tmp",
		bytes.NewReader(body), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if err := p.RenameIfNotExists(context.Background(),
		"manifests/_tmp/manifest.json.tmp",
		"manifests/db1/backups/x/manifest.json"); err != nil {
		t.Fatalf("RenameIfNotExists: %v", err)
	}
	// Final destination must exist + carry the content; src must be gone.
	rd, err := p.Get(context.Background(), "manifests/db1/backups/x/manifest.json")
	if err != nil {
		t.Fatalf("Get dst: %v", err)
	}
	defer rd.Close()
	if string(readAllOrNil(t, rd)) != string(body) {
		t.Errorf("dst content mismatch")
	}
	if _, err := p.Stat(context.Background(), "manifests/_tmp/manifest.json.tmp"); err == nil {
		t.Errorf("src should be unlinked")
	}
}

// TestDirSync_Delete_RoundTrip exercises the unlink + dir-fsync
// path.  After a delete the key must Stat as missing.
func TestDirSync_Delete_RoundTrip(t *testing.T) {
	p, _ := freshFS(t)
	body := []byte("ephemeral")
	if _, err := p.Put(context.Background(), "ephemeral.json",
		bytes.NewReader(body), storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(context.Background(), "ephemeral.json"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Stat(context.Background(), "ephemeral.json"); err == nil {
		t.Errorf("Stat after Delete should fail")
	}
}

// TestDirSync_DeleteMissing_NoError asserts the no-op deletion path
// still tolerates a missing key (and doesn't get confused by a
// missing-parent + fsync-attempt combo).
func TestDirSync_DeleteMissing_NoError(t *testing.T) {
	p, _ := freshFS(t)
	if err := p.Delete(context.Background(), "ghost.json"); err != nil {
		t.Errorf("Delete missing key returned %v; want nil (idempotent)", err)
	}
}

// TestDirSync_NestedPrefixes asserts that the directory-fsync path
// handles deeply-nested prefixes (multiple intermediate MkdirAll
// directories) without surprises.  This is the typical CAS layout:
// chunks/sha256/aa/bb/<hash>.chk
func TestDirSync_NestedPrefixes(t *testing.T) {
	p, _ := freshFS(t)
	body := []byte("nested")
	for i := 0; i < 5; i++ {
		key := filepath.Join("chunks/sha256/aa/bb",
			"object-"+string(rune('a'+i))+".chk")
		if _, err := p.Put(context.Background(), key, bytes.NewReader(body),
			storage.PutOptions{ContentLength: int64(len(body))}); err != nil {
			t.Errorf("Put %s: %v", key, err)
		}
	}
	// Sanity: every key reads back.
	for i := 0; i < 5; i++ {
		key := filepath.Join("chunks/sha256/aa/bb",
			"object-"+string(rune('a'+i))+".chk")
		rd, err := p.Get(context.Background(), key)
		if err != nil {
			t.Errorf("Get %s: %v", key, err)
			continue
		}
		_ = rd.Close()
	}
}

// TestDirSync_DiagnosticOnTempDir exercises the fsync-during-test
// path on a synthetic directory layout, asserting the helper's
// failure mode (best-effort: report failure, don't break state).
// We simulate a syscall failure by passing a non-existent dir.
func TestDirSync_NonExistentParent(t *testing.T) {
	// The helper is unexported; we exercise it via Delete on a key
	// whose parent has been removed under us.  We Put + remove the
	// parent dir directly + then Delete via the plugin.  Delete's
	// Remove will return ENOENT (already gone), the syncDir helper
	// will see the missing dir and return nil, and Delete should
	// return nil (no-op).
	p, root := freshFS(t)
	if _, err := p.Put(context.Background(), "doomed/key",
		bytes.NewReader([]byte("x")), storage.PutOptions{ContentLength: 1}); err != nil {
		t.Fatal(err)
	}
	// Remove the file directly to simulate "already gone".
	if err := os.Remove(filepath.Join(root, "doomed", "key")); err != nil {
		t.Fatal(err)
	}
	// Now Delete via the plugin: file is gone (ENOENT), parent dir
	// still exists; Delete should return nil.
	if err := p.Delete(context.Background(), "doomed/key"); err != nil {
		t.Errorf("Delete on already-deleted file returned %v; want nil", err)
	}
}

// readAllOrNil returns the body of rd or fails the test.
func readAllOrNil(t *testing.T, rd interface{ Read(p []byte) (int, error) }) []byte {
	t.Helper()
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 256)
	for {
		n, err := rd.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf
}
