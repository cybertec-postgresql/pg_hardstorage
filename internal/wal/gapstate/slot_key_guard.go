//go:build !mutation_gap_slot_key_dropped

package gapstate

import (
	"crypto/sha256"
	"encoding/hex"
)

// slot_key_guard.go — gap records are per-slot, but a single Patroni
// failover reconciles every configured slot in the same instant, so two
// slots can produce the same (deployment, tli, unix-nano). The per-slot
// token in the storage key is what keeps their records from colliding on
// an IfNotExists PUT — a collision silently drops the second slot's gap
// and restore loses its ability to refuse a PITR into it. Own file so the
// mutation registry can carry the variant that drops the token.
func slotKeyToken(slotName string) string {
	sum := sha256.Sum256([]byte(slotName))
	return hex.EncodeToString(sum[:8]) // 64-bit — collision-free for any real slot set
}
