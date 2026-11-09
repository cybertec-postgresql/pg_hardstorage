//go:build !windows

package fsutil

import "os"

// SyncDirHandle fsyncs an open directory handle. On Linux and macOS
// this is the real fsync(2) against the directory fd, which is what
// makes a preceding rename/unlink/link durable.
func SyncDirHandle(d *os.File) error {
	return d.Sync()
}
