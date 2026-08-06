//go:build mutation_exit_route_undocumented

package output

import "strings"

// MUTATED variant of the exit routing tables. Identical to the real
// ones except for two extra entries that docs/reference/exit-codes.md
// does not list: a namespace route (`quarantine.*` → ExitConflict) and
// a leaf route (`storage.no_space` → ExitUnreachable).
//
// This is not hypothetical. TestExitCodes_NoUndocumentedRoutes used to
// iterate two hand-written lists of routes with a comment claiming that
// "adding a case there without adding it here fails this test". It did
// not, and could not: the loops only ever visited names someone had
// already written down. Adding exactly this `quarantine` case left the
// whole package green, and a namespace could ship with an exit code no
// operator could look up.
//
// `go test -tags=mutation_exit_route_undocumented ./internal/output/`
// MUST fail — once for the namespace, once for the leaf.
//
// Note the docs→code tests (DocumentedPrefixesMatchCode,
// DocumentedLeavesMatchCode) stay GREEN under this mutation: nothing
// they check moved. Only the code→docs direction catches it, which is
// exactly the direction that was missing.
var exitNamespaceRoutes = map[string]ExitCode{
	"auth":       ExitAuth,
	"usage":      ExitMisuse,
	"preflight":  ExitPreflight,
	"aborted":    ExitAborted,
	"notfound":   ExitNotFound,
	"conflict":   ExitConflict,
	"verify":     ExitVerifyFailed,
	"anomaly":    ExitVerifyFailed,
	"doctor":     ExitDoctorIssues,
	"quarantine": ExitConflict, // <-- undocumented namespace route
}

var exitLeafRoutes = map[string]ExitCode{
	"storage.unreachable":        ExitUnreachable,
	"kms.unreachable":            ExitUnreachable,
	"storage.no_space":           ExitUnreachable, // <-- undocumented leaf route
	"restore.target_unreachable": ExitConflict,
	"restore.target_in_wal_gap":  ExitConflict,
}

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
