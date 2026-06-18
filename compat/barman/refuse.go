// refuse.go — Barman shim refusal helpers: canonical "not implemented" / dropped-flag messages + shimError.
package barman

import (
	"fmt"
	"io"
)

// notImplementedExitCode is the non-zero exit code the shim returns
// when an unsupported verb / flag is invoked.  We use 2 (Cobra's
// usage-error code) so wrapper scripts that test for "tool exited
// non-zero" behave the same as they would against real Barman.
const notImplementedExitCode = 2

// refuseUnimplemented prints the canonical refusal line ("not
// implemented in v1.1; native equivalent: ...") to w and returns the
// error so callers can also bubble it up to Cobra.  Single helper so
// every refusal site speaks the same dialect.
func refuseUnimplemented(w io.Writer, command, suggestion string) error {
	msg := fmt.Sprintf(
		"pg-hardstorage-barman: %s: not implemented in v1.1; native equivalent: %s",
		command, suggestion,
	)
	fmt.Fprintln(w, msg)
	return &shimError{exitCode: notImplementedExitCode, message: msg}
}

// warnDroppedFlag prints a non-fatal warning when a Barman flag has
// no native equivalent because the underlying behaviour differs.
// The shim still proceeds; the operator's existing scripts keep
// working with the new (better) default.
func warnDroppedFlag(w io.Writer, flag, rationale string) {
	fmt.Fprintf(w, "pg-hardstorage-barman: warning: %s ignored — %s\n", flag, rationale)
}

// shimError carries an exit code so cmd/pg-hardstorage-barman's
// main can return the right process status.  The string form is the
// already-printed refusal, repeated for callers that join stderr.
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
