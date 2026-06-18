//go:build unix

package fs

import (
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// statfsFreeSpace probes the volume hosting `path` via statfs(2)
// and returns the volume's TotalBytes + AvailableBytes.
//
// Bavail is the free-block count an UNPRIVILEGED process can use
// — that's what the pgbackup user's backup writes hit. Using
// Bfree (root-includable) would over-report on volumes with
// reserved-for-root blocks (default ext4 reserves 5%).
//
// Bsize on Linux is the optimal-IO block size, but f_bsize on
// macOS / BSD is the fundamental block size; multiplying Bavail
// × Bsize is correct on every Unix today.
func statfsFreeSpace(path string) (storage.FreeSpaceInfo, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return storage.FreeSpaceInfo{}, fmt.Errorf("fs: statfs %q: %w", path, err)
	}
	bsize := int64(st.Bsize)
	return storage.FreeSpaceInfo{
		TotalBytes:     int64(st.Blocks) * bsize,
		AvailableBytes: int64(st.Bavail) * bsize,
	}, nil
}
