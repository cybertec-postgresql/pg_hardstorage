//go:build windows

package fsutil

import "os"

// SyncDirHandle is a no-op on Windows.
//
// Go's *os.File.Sync is FlushFileBuffers, which needs a handle opened
// with write access; os.Open gives a directory handle read-only, so the
// call fails with ERROR_ACCESS_DENIED ("Access is denied") on every
// directory. NTFS journals directory metadata itself, and there is no
// user-space call that flushes one directory's entries; etcd/bbolt and
// Prometheus make the same choice. The file-level fsyncs that precede
// every directory sync in this code base run unchanged on Windows.
func SyncDirHandle(_ *os.File) error {
	return nil
}
