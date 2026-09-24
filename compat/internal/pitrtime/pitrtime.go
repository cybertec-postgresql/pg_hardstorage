// Package pitrtime guards the recovery-target times the compat shims
// forward to native `restore --to`.
//
// Barman and pgBackRest hand a target time straight to PostgreSQL as
// recovery_target_time, and PostgreSQL resolves an offset-less literal
// ("2026-04-27 09:42:00") in the server's TimeZone GUC. The native
// parser (internal/restore/naturaltime) resolves the same literal as
// UTC. Forwarding it verbatim therefore moved the stop point by the
// server's UTC offset — on a Europe/Vienna server a restore meant to
// stop just before a 09:42 local DROP TABLE replayed through 11:42 and
// included it. The shim cannot learn the source server's TimeZone
// reliably at restore time (the server is usually down), so it refuses
// the ambiguous spelling and asks for an explicit offset.
package pitrtime

import (
	"fmt"
	"regexp"
	"strings"
)

// offsetLess matches the absolute date / date-time shapes the native
// parser accepts WITHOUT a zone and silently reads as UTC. Relative
// expressions ("5 minutes ago") and anything carrying an offset,
// "Z" or a UTC alias do not match and pass through untouched.
var offsetLess = regexp.MustCompile(
	`^\d{4}-\d{2}-\d{2}(?:[ T]\d{1,2}:\d{2}(?::\d{2}(?:\.\d+)?)?)?$`)

// RequireExplicitZone returns an error when value is an absolute
// timestamp without a UTC offset. flag names the operator's own flag
// in the message, and tool the upstream tool whose semantics differ.
func RequireExplicitZone(tool, flag, value string) error {
	v := strings.TrimSpace(value)
	if !offsetLess.MatchString(v) {
		return nil
	}
	return fmt.Errorf(
		"%s %q has no UTC offset: %s lets PostgreSQL read it in the server's TimeZone, "+
			"pg_hardstorage would read it as UTC, and the recovery would stop at a different instant; "+
			"add the offset explicitly, e.g. %q or %q",
		flag, value, tool, v+"+00", v+"+02:00")
}
