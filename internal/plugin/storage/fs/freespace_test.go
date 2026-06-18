package fs_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// TestFS_FreeSpace_HappyPath: fs.Plugin reports a non-zero
// AvailableBytes for a real tempdir on a real filesystem.
// We don't assert exact values — those depend on the host —
// just that the probe succeeds and returns plausible numbers.
func TestFS_FreeSpace_HappyPath(t *testing.T) {
	root := t.TempDir()
	p := &fs.Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	info, err := p.FreeSpace(context.Background())
	if err != nil {
		t.Fatalf("FreeSpace: %v", err)
	}
	if info.Unsupported {
		t.Errorf("fs.Plugin should support FreeSpace on this platform")
	}
	if info.AvailableBytes <= 0 {
		t.Errorf("AvailableBytes = %d, want > 0", info.AvailableBytes)
	}
	if info.TotalBytes < info.AvailableBytes {
		t.Errorf("TotalBytes = %d < AvailableBytes = %d (impossible)",
			info.TotalBytes, info.AvailableBytes)
	}
}

// TestFS_FreeSpace_BeforeOpen_Errors: calling FreeSpace on a
// Plugin that hasn't been Opened is a programmer error.
func TestFS_FreeSpace_BeforeOpen_Errors(t *testing.T) {
	p := &fs.Plugin{}
	_, err := p.FreeSpace(context.Background())
	if err == nil {
		t.Fatal("FreeSpace before Open should error")
	}
}

// TestFreeSpaceOf_FSImplements: storage.FreeSpaceOf finds the
// FreeSpaceAware interface on fs.Plugin and returns a real
// info struct.
func TestFreeSpaceOf_FSImplements(t *testing.T) {
	root := t.TempDir()
	p := &fs.Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: root},
	}); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	info, err := storage.FreeSpaceOf(context.Background(), p)
	if err != nil {
		t.Fatalf("FreeSpaceOf: %v", err)
	}
	if info.Unsupported {
		t.Error("fs.Plugin should not report Unsupported via FreeSpaceOf")
	}
}
