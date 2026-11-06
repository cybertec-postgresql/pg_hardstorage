package sftp_test

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract"
)

// TestSFTP_PruneEmptyDirs runs the EmptyDirPruner clauses and the
// writers-against-pruners race on a real OpenSSH sftp-server
// (atmoz/sftp). The in-process tests cover the logic; this one pins
// what only the real server can — which SFTP status it sends for a
// write into a directory that is gone (the retry's trigger) and for an
// rmdir of a directory that is no longer empty (not an error).
func TestSFTP_PruneEmptyDirs(t *testing.T) {
	p := openSFTPOnFresh(t)
	contract.PruneEmptyDirs(t, p)
	iters := 60
	if testing.Short() {
		iters = 15
	}
	contract.PruneRacesWriters(t, p, 4, iters)
}
