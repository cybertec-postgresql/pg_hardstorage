//go:build !unix

package fs

import (
	"errors"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// statfsFreeSpace on non-Unix platforms reports unsupported.
// Today's tooling targets Linux primarily; Windows builds (the
// CLI is built for windows/amd64 per the plan) don't have a
// drop-in statfs equivalent in golang.org/x/sys, and
// capacity-preflight is fail-open on Unsupported anyway —
// returning Unsupported here is the right semantic.
func statfsFreeSpace(path string) (storage.FreeSpaceInfo, error) {
	return storage.FreeSpaceInfo{Unsupported: true}, errors.New("fs: free-space probe is unsupported on this platform")
}
