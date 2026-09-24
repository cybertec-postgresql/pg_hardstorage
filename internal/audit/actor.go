// actor.go — default attribution for audit events that name no actor.
package audit

import (
	"os"
	"os/user"
	"strings"
)

// EnvActor overrides the default actor attributed to audit events whose
// caller set none — for a service account or a wrapper that knows the
// human behind the run better than the OS login does.
const EnvActor = "PG_HARDSTORAGE_ACTOR"

// DefaultActor returns the identity Append records on an event that
// carries no Actor: $PG_HARDSTORAGE_ACTOR if set, otherwise the login
// name ($USER, $LOGNAME — what `approval request` records as the
// initiator — else the OS account) qualified with the host name, as
// "user@host". Never empty: "unknown@<host>" (or "unknown") is still an
// actor, and insider detection can profile it, whereas "" is skipped.
//
// Read per call, not cached, so a long-running agent picks up a
// changed environment the same way a fresh CLI invocation would.
func DefaultActor() string {
	if a := strings.TrimSpace(os.Getenv(EnvActor)); a != "" {
		return a
	}
	name := strings.TrimSpace(os.Getenv("USER"))
	if name == "" {
		name = strings.TrimSpace(os.Getenv("LOGNAME"))
	}
	if name == "" {
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
	}
	if name == "" {
		name = "unknown"
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return name + "@" + host
	}
	return name
}
