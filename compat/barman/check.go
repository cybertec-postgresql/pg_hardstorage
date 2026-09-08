// check.go — Barman shim verb: `barman check <server>` → native `pg_hardstorage doctor <server>` (incl. --nagios).
package barman

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// newCheckCmd handles `barman check <server>`.
//
// Native dispatch: `pg_hardstorage doctor <server>`.
//
// Barman's `check` and the native `doctor` overlap heavily —
// both audit connectivity, replication slot health, repository
// reachability, and config sanity.  The native doctor reports a few
// extra items (KEK presence, manifest signature roots) that Barman
// users will see as "more checks, same shape".
//
// # --nagios
//
// Monitoring is the whole reason `barman check --nagios` exists, and
// this path was broken in every direction at once:
//
//   - the template read `.ok` and `.summary`, which the doctor result
//     does not have (its fields are `healthy` and `issues`), so it
//     rendered the literal `<no value>` and always took the CRITICAL
//     branch — a healthy server paged;
//   - doctor was invoked without --exit-on-issues, so the process
//     exited 0 in every state — an unhealthy server monitored green,
//     which is the exact failure Nagios checks exist to catch.
//
// Rather than templating over a shape that can drift again, the shim
// now asks for JSON and renders the Nagios line itself from the real
// fields, mapping the verdict onto Nagios' own exit codes (0 OK,
// 2 CRITICAL) the way real barman does.
func newCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	var nagios bool
	c := &cobra.Command{
		Use:   "check <server>",
		Short: "Health-check a server (Barman compat)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			server := args[0]
			// Native `doctor [<deployment>]` takes the deployment as a
			// positional and registers NO --repo / --pg-connection flags
			// (only --exit-on-issues plus the persistent --output/--template).
			// Do NOT inject deployment flags here — cobra rejects them as
			// unknown. The server name is already the doctor positional,
			// and doctor scopes its probes to it.
			if !nagios {
				return dispatchNative(stdout, stderr, []string{"doctor", server})
			}
			return runNagiosCheck(stdout, stderr, server)
		},
	}
	c.Flags().BoolVar(&nagios, "nagios", false, "Barman: emit a single-line Nagios-format result")
	c.SilenceUsage = true
	return c
}

// Nagios plugin exit codes. Real barman uses the same set, and every
// Nagios/Icinga wrapper branches on them.
const (
	nagiosWarning  = 1
	nagiosCritical = 2
	nagiosUnknown  = 3
)

// doctorNagiosView is the slice of the doctor result the Nagios line
// needs. Decoding a named struct (rather than templating over a map)
// means a field rename in doctor breaks the build here instead of
// silently rendering "<no value>" to a monitoring system.
type doctorNagiosView struct {
	Result struct {
		Healthy bool `json:"healthy"`
		Issues  []struct {
			Severity string `json:"severity"`
			Code     string `json:"code"`
			Message  string `json:"message"`
		} `json:"issues"`
	} `json:"result"`
}

// runNagiosCheck dispatches doctor with JSON output, captures it, and
// writes a single Nagios-shaped line plus the matching exit code.
func runNagiosCheck(stdout, stderr io.Writer, server string) error {
	// Capture BOTH streams: the result envelope goes to stdout, but a
	// failure envelope (unknown server, unreadable config) is written
	// to stderr, and a Nagios check that silently dropped that would
	// be back to reporting a broken server as fine.
	var outBuf, errBuf bytes.Buffer
	dispatchErr := dispatchNative(&outBuf, &errBuf, []string{"doctor", server, "--output", "json"})

	view, ok := decodeDoctorReport(outBuf.Bytes())
	if !ok {
		// No report means doctor could not run at all. That is
		// UNKNOWN (Nagios 3), never OK — and the operator needs the
		// reason on the line.
		detail := doctorErrorMessage(errBuf.Bytes())
		if detail == "" {
			detail = doctorErrorMessage(outBuf.Bytes())
		}
		if detail == "" && dispatchErr != nil {
			detail = dispatchErr.Error()
		}
		if detail == "" {
			detail = "doctor produced no parseable report"
		}
		fmt.Fprintf(stdout, "BARMAN UNKNOWN - server %s: %s\n", server, firstLine(detail))
		return &shimError{exitCode: nagiosUnknown, message: ""}
	}

	// Match native doctor's own --exit-on-issues threshold: notice and
	// info are informational and must not page anyone.
	actionable := 0
	for _, i := range view.Result.Issues {
		if severityRank(i.Severity) >= severityRank("warning") {
			actionable++
		}
	}

	if view.Result.Healthy && actionable == 0 {
		fmt.Fprintf(stdout, "BARMAN OK - server %s: all checks passed\n", server)
		return nil
	}

	// Summarise from the real issues. Nagios reads the first line, so
	// lead with the count and the highest-severity message.
	summary := summariseIssues(server, view.Result.Issues)
	if view.Result.Healthy {
		// Warnings but nothing that makes the deployment unhealthy.
		fmt.Fprintf(stdout, "BARMAN WARNING - %s\n", summary)
		return &shimError{exitCode: nagiosWarning, message: ""}
	}
	fmt.Fprintf(stdout, "BARMAN CRITICAL - %s\n", summary)
	return &shimError{exitCode: nagiosCritical, message: ""}
}

// decodeDoctorReport pulls the doctor result out of a stream that may
// carry warning EVENTS before the result envelope (the JSON renderer
// emits one object per event, then the result). It scans every
// top-level object and keeps the one that actually carries a report.
func decodeDoctorReport(b []byte) (doctorNagiosView, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	var found doctorNagiosView
	var ok bool
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			break
		}
		var probe struct {
			Result *json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil || probe.Result == nil {
			continue
		}
		var view doctorNagiosView
		if err := json.Unmarshal(raw, &view); err != nil {
			continue
		}
		found, ok = view, true
	}
	return found, ok
}

// doctorErrorMessage extracts the structured error message from a
// pg_hardstorage envelope, so the Nagios line can name the real
// failure instead of "no parseable report".
func doctorErrorMessage(b []byte) string {
	dec := json.NewDecoder(bytes.NewReader(b))
	for {
		var env struct {
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := dec.Decode(&env); err != nil {
			return ""
		}
		if env.Error != nil && env.Error.Message != "" {
			if env.Error.Code != "" {
				return fmt.Sprintf("[%s] %s", env.Error.Code, env.Error.Message)
			}
			return env.Error.Message
		}
	}
}

// summariseIssues renders "<n> issue(s): <worst message>" from the
// doctor issue list, preferring the most severe entry so the single
// line a pager shows names the real problem.
func summariseIssues(server string, issues []struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}) string {
	if len(issues) == 0 {
		return fmt.Sprintf("server %s: unhealthy (no issue detail reported)", server)
	}
	worst := issues[0]
	for _, i := range issues[1:] {
		if severityRank(i.Severity) > severityRank(worst.Severity) {
			worst = i
		}
	}
	return fmt.Sprintf("server %s: %d issue(s), worst %s [%s]: %s",
		server, len(issues), worst.Severity, worst.Code, firstLine(worst.Message))
}

// severityRank orders the severity names doctor emits. Unknown names
// sort lowest so a new name never displaces a known critical.
func severityRank(s string) int {
	switch strings.ToLower(s) {
	case "emergency", "alert":
		return 5
	case "critical":
		return 4
	case "error":
		return 3
	case "warning":
		return 2
	case "notice", "info":
		return 1
	default:
		return 0
	}
}

// firstLine keeps a Nagios line to one line: the protocol is
// line-oriented and a multi-line message corrupts the plugin output.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}
