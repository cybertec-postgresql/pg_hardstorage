//go:build mutation_fetch_sysid_unchecked

package cli

// checkFetchSystemIdentifier — MUTATED variant: the identity check
// does not exist (the pre-#26 world). A foreign segment is handed to
// PostgreSQL, which discovers the mismatch only mid-replay with a
// FATAL that names neither the deployment nor the repair — and the
// operator has already paid the restore's wallclock.

func checkFetchSystemIdentifier(expect, got, segmentName string) error {
	return nil
}
