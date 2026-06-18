// refuse.go — WAL-G shim refusal helpers: canonical "not implemented" / dropped-flag messages + shimError.
package walg

import (
	"fmt"
	"io"
)

// notImplementedExitCode is the non-zero exit code the shim returns
// when an unsupported verb / flag is invoked.  We use 2 (Cobra's
// usage-error code) so wrapper scripts that test for "tool exited
// non-zero" behave the same as they would against real WAL-G — and
// match the pgBackRest + Barman shims.
const notImplementedExitCode = 2

// refuseUnimplemented prints the canonical refusal line ("not
// implemented in v1.1; native equivalent: ...") to w and returns the
// error so callers can also bubble it up to Cobra.
func refuseUnimplemented(w io.Writer, command, suggestion string) error {
	msg := fmt.Sprintf(
		"pg-hardstorage-walg: %s: not implemented in v1.1; native equivalent: %s",
		command, suggestion,
	)
	fmt.Fprintln(w, msg)
	return &shimError{exitCode: notImplementedExitCode, message: msg}
}

// warnDroppedFlag prints a non-fatal warning when a WAL-G env var has
// no native equivalent because the underlying behaviour differs.
// The shim still proceeds; the operator's existing scripts keep
// working with the new (better) default.
func warnDroppedFlag(w io.Writer, name, rationale string) {
	fmt.Fprintf(w, "pg-hardstorage-walg: warning: %s ignored — %s\n", name, rationale)
}

// shimError carries an exit code so cmd/pg-hardstorage-walg's main
// can return the right process status.  Mirrors compat/barman and
// compat/pgbackrest.
type shimError struct {
	exitCode int
	message  string
}

// Error returns the pre-formatted refusal message.
func (e *shimError) Error() string { return e.message }

// ExitCode returns the requested process exit code, or 1 if the
// error is some other type that didn't carry one.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if se, ok := err.(*shimError); ok {
		return se.exitCode
	}
	return 1
}
