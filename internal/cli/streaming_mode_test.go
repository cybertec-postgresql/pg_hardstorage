package cli

import (
	"os"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// PGHS_TEST_STREAM_ALL=1 makes every *Auto manifest read take the
// streamed path, so the whole suite also exercises it (not just the
// huge-manifest case it exists for).
func init() {
	if os.Getenv("PGHS_TEST_STREAM_ALL") == "1" {
		backup.AutoStreamThreshold = -1
	}
}
