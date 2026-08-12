//go:build mutation_dek_retention_dropped

package sharedkey

import (
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// dekRetention — MUTATED: drops the WORM deadline, restoring the pre-fix
// world where the shared DEK is written with no RetainUntil. On a
// compliance repo the DEK is then deletable while everything it decrypts
// stays locked — one delete loses all encrypted data.
func dekRetention(_ time.Time, _ storage.WORMMode) (time.Time, storage.WORMMode) {
	return time.Time{}, ""
}
