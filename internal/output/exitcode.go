// exitcode.go — stable v1 CLI exit-code contract and error→code
// classification (see docs/SPEC.md).

package output

import (
	"errors"
	"strings"
)

// ExitCode is the stable exit-code contract documented in docs/SPEC.md.
//
// These values are part of the v1 contract and must not change without a
// major-version bump. Scripts and CI rely on them.
type ExitCode int

const (
	// ExitOK — the command completed successfully and produced its
	// expected output.
	ExitOK ExitCode = 0
	// ExitError — generic failure bucket for any error that does not
	// classify into a more specific exit code below.
	ExitError ExitCode = 1
	// ExitMisuse — bad CLI arguments (cobra flag-parse failures,
	// missing positional args, unknown --output value, etc.); any
	// error wrapping ErrUsage maps here.
	ExitMisuse ExitCode = 2
	// ExitAuth — authentication or authorization failure (errors with
	// the "auth.*" code namespace, e.g. auth.denied / auth.token_expired).
	ExitAuth ExitCode = 3
	// ExitPreflight — a pre-flight check failed and no mutation
	// occurred (errors with the "preflight.*" code namespace, e.g.
	// preflight.disk_full / preflight.repo_locked).
	ExitPreflight ExitCode = 4
	// ExitAborted — the operator aborted the operation (Ctrl-C on an
	// interactive prompt, "aborted.*" coded errors).
	ExitAborted ExitCode = 5
	// ExitNotFound — a requested resource (backup, manifest,
	// deployment) was not found ("notfound.*" code namespace).
	ExitNotFound ExitCode = 6
	// ExitConflict — a conflicting operation is in progress (lease
	// held by another writer, in-progress restore, etc.; "conflict.*"
	// namespace).
	ExitConflict ExitCode = 7
	// ExitUnreachable — the storage backend or KMS is unreachable
	// (specific leaf codes storage.unreachable / kms.unreachable);
	// other storage/kms errors stay in ExitError.
	ExitUnreachable ExitCode = 8
	// ExitVerifyFailed — a backup-verification or anomaly check
	// produced a failure ("verify.*" / "anomaly.*" code namespaces);
	// used so cron-driven checks fire non-zero exit alarms.
	ExitVerifyFailed ExitCode = 9
	// ExitDoctorIssues — `pg_hardstorage doctor --exit-on-issues`
	// reports one or more remediation items ("doctor.*" namespace).
	ExitDoctorIssues ExitCode = 10
)

// ErrUsage is sentinel for "the user invoked the CLI wrong"; the CLI maps
// this to ExitMisuse. cobra-internal errors (unknown flag, missing arg)
// should be wrapped with this so the CLI can detect them uniformly.
var ErrUsage = errors.New("usage error")

// ExitCodeFor classifies an error for the process exit code.
//
// Resolution order:
//  1. If err == nil: ExitOK.
//  2. If err is or wraps ErrUsage: ExitMisuse.
//  3. If err carries a structured *Error: codePrefixToExit(code).
//  4. Otherwise: ExitError.
//
// We intentionally do not let arbitrary errors leak into specific exit
// codes — only structured *Error can claim a non-generic exit code.
func ExitCodeFor(err error) ExitCode {
	if err == nil {
		return ExitOK
	}
	if errors.Is(err, ErrUsage) {
		return ExitMisuse
	}
	if oe, ok := AsOutputError(err); ok {
		return codePrefixToExit(oe.Code)
	}
	return ExitError
}

// codePrefixToExit maps an error-code prefix to an exit code.
//
// Codes are dotted, lowercase strings: "wal.slot_missing", "auth.denied",
// "preflight.disk_full", "verify.checksum_mismatch", etc. The first dotted
// segment is the namespace and drives the mapping.
//
// New namespaces should be added as the system grows; until they are,
// they fall through to ExitError. That's the safe default.
func codePrefixToExit(code string) ExitCode {
	ns, _, _ := strings.Cut(code, ".")
	switch ns {
	case "auth":
		return ExitAuth
	case "usage":
		return ExitMisuse
	case "preflight":
		return ExitPreflight
	case "aborted":
		return ExitAborted
	case "notfound":
		return ExitNotFound
	case "conflict":
		return ExitConflict
	case "verify":
		return ExitVerifyFailed
	case "anomaly":
		// Same posture as verify: a baseline-shift finding flips the
		// exit code so cron-driven `anomaly check` alarms. The
		// finding is not a verification failure per se — the backup
		// itself is fine — but operationally the operator wants the
		// same "non-zero exit if something is unusual" cron contract.
		return ExitVerifyFailed
	case "doctor":
		return ExitDoctorIssues
	}
	// "storage.*" / "kms.*" only count as Unreachable when the leaf code
	// is specifically about reachability. Other storage/kms errors stay
	// in the generic-error bucket.
	switch code {
	case "storage.unreachable", "kms.unreachable":
		return ExitUnreachable
	// "restore.target_*" leaves are conflict-class — the operator's
	// chosen PITR target conflicts with the backup's available
	// range. Cron-driven restores can then distinguish "config
	// error, fix your --to-lsn" from "transient infrastructure
	// failure" by exit code alone.  Other restore.* leaves stay
	// in the generic-error bucket.
	case "restore.target_unreachable",
		"restore.target_in_wal_gap":
		return ExitConflict
	}
	return ExitError
}
