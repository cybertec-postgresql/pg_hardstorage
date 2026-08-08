//go:build !mutation_wal_removed_retried

package cli

// wal_removed_error.go — recognising PostgreSQL's own "that WAL is
// gone" verdict.
//
// The predictive start-vs-restart_lsn refusal was retired after the
// chaos gate's dcs_outage fault proved it kills streamers in
// self-healing situations (Patroni recreates permanent slots at the
// promotion point, so a recreated slot routinely sits above a
// perfectly servable archive frontier). The stream now ATTEMPTS the
// resume and this matcher recognises the evidence-based terminal
// case: walsender refusing because the segment is genuinely recycled.
// decideStreamStop maps it to the classic
// wal.start_before_slot_restart_lsn code and remediation, so the
// operator experience of a REAL loss is unchanged — only the false
// positives are gone. Own file so the mutation registry can carry the
// match-nothing variant (which would retry the unfixable forever —
// issue #45's exact shape for this error).

import "strings"

// isWalRemovedError matches the walsender error family for recycled
// WAL. PostgreSQL's wording (src/backend/access/transam/xlogutils.c
// and walsender.c) has been stable across supported majors:
//
//	requested WAL segment 000000010000000000000005 has already been removed
//
// Substring-matched because the error arrives wrapped in transport
// and CLI layers with the segment name embedded.
func isWalRemovedError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "has already been removed")
}
