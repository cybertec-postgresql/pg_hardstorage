//go:build !mutation_exit_route_undocumented

package output

import "strings"

// exitNamespaceRoutes maps an error-code NAMESPACE (the first dotted
// segment) to its exit code. Anything absent falls through to
// ExitError, which is the safe default for a namespace nobody has
// classified yet.
//
// This is a map rather than a switch so the contract is ENUMERABLE.
// TestExitCodes_NoUndocumentedRoutes walks it directly and requires a
// matching row in docs/reference/exit-codes.md, so a route added here
// without a doc row fails.
//
// The previous shape was a switch, and the test kept two hand-written
// lists of its cases with a comment claiming that adding a case without
// updating the lists would fail. It could not — the loops only visited
// names already written down, so a namespace could ship with an exit
// code no operator could look up. Reading the table itself is the only
// version that keeps the promise. An AST scan reads a fixed file and
// cannot see build tags; a map can be walked at run time, under
// whatever tags the binary was built with.
var exitNamespaceRoutes = map[string]ExitCode{
	"auth":      ExitAuth,
	"usage":     ExitMisuse,
	"preflight": ExitPreflight,
	"aborted":   ExitAborted,
	"notfound":  ExitNotFound,
	"conflict":  ExitConflict,
	"verify":    ExitVerifyFailed,

	// Same posture as verify: a baseline-shift finding flips the exit
	// code so cron-driven `anomaly check` alarms. The finding is not a
	// verification failure per se — the backup itself is fine — but
	// operationally the operator wants the same "non-zero exit if
	// something is unusual" cron contract.
	"anomaly": ExitVerifyFailed,

	"doctor": ExitDoctorIssues,
}

// exitLeafRoutes maps a WHOLE error code to an exit code, for cases
// where the namespace alone is too coarse.
//
// "storage.*" / "kms.*" only count as Unreachable when the leaf code is
// specifically about reachability; other storage/kms errors stay in the
// generic-error bucket. "restore.target_*" leaves are conflict-class —
// the operator's chosen PITR target conflicts with the backup's
// available range, so a cron-driven restore can tell "config error, fix
// your --to-lsn" from "transient infrastructure failure" by exit code
// alone. Other restore.* leaves stay generic.
var exitLeafRoutes = map[string]ExitCode{
	"storage.unreachable":        ExitUnreachable,
	"kms.unreachable":            ExitUnreachable,
	"restore.target_unreachable": ExitConflict,
	"restore.target_in_wal_gap":  ExitConflict,
}

// codePrefixToExit maps an error code to an exit code.
//
// Codes are dotted, lowercase strings: "wal.slot_missing", "auth.denied",
// "preflight.disk_full", "verify.checksum_mismatch", etc. The namespace
// is consulted first, then the whole-code leaf table, so a leaf route
// sharpens a namespace that has none.
func codePrefixToExit(code string) ExitCode {
	ns, _, _ := strings.Cut(code, ".")
	if ec, ok := exitNamespaceRoutes[ns]; ok {
		return ec
	}
	if ec, ok := exitLeafRoutes[code]; ok {
		return ec
	}
	return ExitError
}
