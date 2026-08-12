//go:build !mutation_dek_retention_dropped

package sharedkey

import (
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// dek_retention_guard.go — the shared DEK decrypts EVERY encrypted chunk in
// the repo; on a WORM repo it must be Object-Locked at least as long as the
// backups it protects, or deleting this one object loses ALL encrypted data
// while the immutable chunks remain unreadable. Own file so the mutation
// registry can carry the variant that drops the lock (the pre-fix hole).
func dekRetention(retainUntil time.Time, mode storage.WORMMode) (time.Time, storage.WORMMode) {
	return retainUntil, mode
}
