//go:build mutation_gap_slot_key_dropped

package gapstate

// slotKeyToken — MUTATED: drops the per-slot token, restoring the pre-fix
// key that omits the slot. Two slots that gap on the same timeline at the
// same instant then share a key; the second slot's IfNotExists PUT is
// rejected as already-present and its gap record is silently lost.
func slotKeyToken(_ string) string { return "" }
