// logs.go — CLI surface for journald log retrieval for the agent unit.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newLogsCmd implements `pg_hardstorage logs [<deployment>]` — the
// 3am-operator's "what was the agent doing?" surface.
//
// Mechanism: thin wrapper over `journalctl` against the unit names
// the SPEC's systemd template ships (`pg_hardstorage.service` for
// the singleton agent, `pg_hardstorage@<deployment>.service` for the
// templated multi-instance form). The wrapper exists for two
// reasons:
//
//  1. UX. Operators don't always remember the unit name; a single
//     `pg_hardstorage logs db1` walks both forms.
//  2. JSON / NDJSON consumption. `--output ndjson` translates
//     journalctl's native -o json output into our wrapped
//     `pg_hardstorage.v1` Event shape so the same monitoring pipeline
//     that consumes `pg_hardstorage backup -o ndjson` can also tail
//     the agent without extra parsing logic.
//
// What's NOT here: a polyfill for non-systemd hosts. macOS uses
// log(1); BSD uses syslogd directly; container deployments scrape
// stdout. Building those would be a Tier-1 logging plugin
// architecture; for v0.1.1 we ship the systemd path —
// the production environment most operators run.
func newLogsCmd() *cobra.Command {
	var (
		follow bool
		lines  int
		since  string
		unit   string
	)
	c := &cobra.Command{
		Use:   "logs [<deployment>]",
		Short: "Tail the pg_hardstorage agent's systemd journal",
		Long: `Wraps journalctl(1) against the agent's systemd unit. With no
deployment argument, follows the singleton ` + "`pg_hardstorage.service`" + `
unit; with a deployment, follows the templated
` + "`pg_hardstorage@<deployment>.service`" + ` unit (the SPEC's
multi-instance pattern).

Options:

  --follow / -f      tail forward (default: print last 100 lines)
  --lines N          how many lines to print initially (default 100)
  --since DUR-OR-TS  start from this point ("24h", "yesterday",
                     RFC3339). A bare duration means "that long
                     ago" and is negated for journalctl, which
                     requires a sign on relative times.
  --unit NAME        override the auto-derived unit name

Requires journalctl on PATH. On non-systemd hosts (macOS, BSD,
some container images) this command exits with usage.no_journalctl;
read the agent's stdout directly or wire a logging plugin.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			deployment := ""
			if len(args) == 1 {
				deployment = args[0]
			}
			return runLogs(cmd, deployment, unit, since, lines, follow)
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false,
		"tail forward indefinitely (Ctrl-C to stop)")
	c.Flags().IntVarP(&lines, "lines", "n", 100,
		"how many lines to print initially")
	c.Flags().StringVar(&since, "since", "",
		"start at this point (24h / yesterday / RFC3339); a bare duration means that long ago")
	c.Flags().StringVar(&unit, "unit", "",
		"override the auto-derived systemd unit name")
	return c
}

// journalSince converts a --since value into something journalctl
// actually accepts.
//
// The flag is documented as DUR-OR-TS with "24h" as the FIRST example,
// and the value was then passed to journalctl untouched. journalctl
// does not accept a bare duration:
//
//	$ journalctl --since 24h
//	Failed to parse timestamp: 24h
//
// so the most obvious way to invoke the flag — the spelling the help
// text itself suggests — failed, and it surfaced as a generic
// `internal` error rather than anything pointing at --since.
//
// systemd wants a SIGN on a relative time ("-24h", or "24h ago").
// A bare duration from an operator can only mean "this long ago", so
// negate it. Every other accepted spelling is left untouched:
// "yesterday", "now", "2026-08-01 10:00:00", "1 hour ago", and values
// that already carry a - or + sign.
//
// Verified against journalctl 257: every duration time.ParseDuration
// accepts is accepted by systemd once negated — fractional ("-1.5h"),
// sub-second ("-300ms", "-100µs") and compound ("-2h45m30s") included
// — so this cannot turn one rejected value into another.
func journalSince(since string) string {
	trimmed := strings.TrimSpace(since)
	if trimmed == "" {
		return since
	}
	// Already signed: the operator spelled the direction themselves.
	if trimmed[0] == '-' || trimmed[0] == '+' {
		return trimmed
	}
	// A bare Go duration means "this long ago".
	if _, err := time.ParseDuration(trimmed); err == nil {
		return "-" + trimmed
	}
	// Anything else is a systemd timestamp spelling; pass it through.
	return since
}

func runLogs(cmd *cobra.Command, deployment, overrideUnit, since string, lines int, follow bool) error {
	d := DispatcherFrom(cmd)

	// Locate journalctl. Failing here is the most common
	// non-systemd-host case; surface a structured error so a
	// monitoring tool can detect "this host doesn't have systemd"
	// vs "the agent isn't running."
	bin, err := exec.LookPath("journalctl")
	if err != nil {
		return output.NewError("usage.no_journalctl",
			"logs: journalctl not found on PATH (this host likely doesn't run systemd)").
			WithSuggestion(&output.Suggestion{
				Human: "on macOS / BSD / some container images journalctl is unavailable. Read the agent's stdout directly, or run `journalctl` equivalent for your platform.",
			}).Wrap(output.ErrUsage)
	}

	unit := overrideUnit
	if unit == "" {
		unit = unitName(deployment)
	}

	args := []string{
		"-u", unit,
		"-o", "short-iso",
		"-n", strconv.Itoa(lines),
	}
	if follow {
		args = append(args, "-f")
	}
	if since != "" {
		args = append(args, "--since", journalSince(since))
	}

	// Mode A: the operator wants tail-style streaming output. We
	// exec journalctl with stdout/stderr inherited so the log
	// stream goes straight to their terminal. The dispatcher's
	// Result/Event mechanism doesn't suit a 24-hour-tail use case.
	if follow || d.Renderer().Name() == "text" {
		c := exec.CommandContext(cmd.Context(), bin, args...)
		c.Stdout = os.Stdout
		// Tee stderr to the terminal AND capture it, so we can tell a
		// benign "no entries" (empty stderr) apart from a real failure
		// (e.g. a bad --since value, whose message journalctl writes to
		// stderr) without hiding the message from the operator.
		var stderrBuf strings.Builder
		c.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)
		if err := c.Run(); err != nil {
			// Exit code 1 is overloaded: "no entries for this unit"
			// (empty stderr) vs a genuine failure (non-empty stderr).
			// Only the former is a structured notfound; a real error
			// like `--since garbage` must surface, not misreport "no
			// entries".
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				if stderr := strings.TrimSpace(stderrBuf.String()); stderr == "" {
					return noEntriesErr(cmd.Context(), unit)
				} else {
					return fmt.Errorf("logs: journalctl: %s", stderr)
				}
			}
			return fmt.Errorf("logs: journalctl: %w", err)
		}
		// journalctl exited 0. On systemd 255 that covers "unit is
		// quiet" AND "unit does not exist", so a clean exit is not
		// proof the unit is real — ask before reporting success.
		return noEntriesErr(cmd.Context(), unit)
	}

	// Mode B: structured output (-o json / ndjson). Ask journalctl
	// for its native -o json (one JSON object per line), then wrap
	// each object in our Result body shape. ndjson mode emits
	// per-line; json mode emits a single Result with all lines.
	args = append(args, "-o", "json")
	c := exec.CommandContext(cmd.Context(), bin, args...)
	out, err := c.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			// Exit 1 is overloaded: journalctl uses it BOTH for "no
			// entries for this unit" (empty stderr) AND for real
			// failures like a bad --since value ("Failed to parse
			// timestamp: garbage"). Only the former is a notfound;
			// otherwise surface the real error + its stderr so
			// `logs --since garbage` doesn't misreport "no entries".
			if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr == "" {
				if nerr := noEntriesErr(cmd.Context(), unit); nerr != nil {
					return nerr
				}
				return d.Result(output.NewResult(cmd.CommandPath()).
					WithBody(logsBody{Unit: unit}))
			} else {
				return fmt.Errorf("logs: journalctl: %s", stderr)
			}
		}
		return fmt.Errorf("logs: journalctl: %w", err)
	}
	body := logsBody{Unit: unit, Lines: parseJournalJSON(string(out))}
	if len(body.Lines) == 0 {
		// Same reasoning as the text path: exit 0 + no lines does not
		// mean the unit exists.
		if nerr := noEntriesErr(cmd.Context(), unit); nerr != nil {
			return nerr
		}
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// unitExists reports whether systemd knows the unit, and whether we
// were able to find out at all.
//
// This is the missing half of "no entries" handling. journalctl exits 0
// with no output BOTH when a unit exists and has simply been quiet AND
// when the unit does not exist — verified against systemd 255:
//
//	journalctl -u systemd-journald.service --since -1us  -> exit 0
//	journalctl -u no-such-unit.service                   -> exit 0
//
// so the exit code cannot separate them and neither can the output.
// systemctl can: LoadState is "not-found" for a unit systemd has never
// heard of and "loaded" for one it knows.
//
// The second return distinguishes "systemd says loaded" from "we could
// not ask" (no systemctl on PATH, a container without a system bus, a
// permission error). Guessing on a failed probe is how the original
// bug happened, so an unavailable probe reports ok=false and the caller
// stays silent rather than inventing an answer.
func unitExists(ctx context.Context, unit string) (exists, ok bool) {
	bin, err := exec.LookPath("systemctl")
	if err != nil {
		return false, false
	}
	out, err := exec.CommandContext(ctx, bin,
		"show", "-p", "LoadState", "--value", unit).Output()
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(string(out)) {
	case "not-found":
		return false, true
	case "":
		return false, false
	default:
		// loaded / masked / bad-setting / error: systemd knows of it.
		return true, true
	}
}

// noEntriesErr is the answer to "journalctl gave us nothing".
//
// Returns a notfound.unit error ONLY when systemd positively reports
// the unit as not-found. A unit that exists and is merely quiet is not
// an error — reporting one would be a worse failure than the silence
// this replaces, because a healthy agent that simply has not logged
// recently would look like a missing one. Same when the probe itself
// is unavailable: nil, and the caller returns an empty result.
func noEntriesErr(ctx context.Context, unit string) error {
	if exists, ok := unitExists(ctx, unit); ok && !exists {
		return output.NewError("notfound.unit",
			fmt.Sprintf("logs: systemd has no unit %q (LoadState=not-found); "+
				"check the deployment name, or pass --unit", unit))
	}
	return nil
}

// unitName derives the systemd unit. Empty deployment maps to the
// singleton service name; non-empty maps to the templated form.
func unitName(deployment string) string {
	if deployment == "" {
		return "pg_hardstorage.service"
	}
	return "pg_hardstorage@" + deployment + ".service"
}

// parseJournalJSON splits journalctl's -o json output (one object
// per line, NUL-terminated in some versions) and returns the message
// + timestamp + priority for each entry. We extract a small subset
// so the Result body stays compact; operators wanting the full
// systemd metadata pass --output json and parse the wrapped form
// (which preserves every field as a journalLine.Raw).
func parseJournalJSON(s string) []journalLine {
	var out []journalLine
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		jl := journalLine{Raw: line}
		// Cheap field extraction without unmarshalling the whole
		// thing — journalctl's JSON is flat and the keys we want
		// (MESSAGE, __REALTIME_TIMESTAMP, PRIORITY) are operator-
		// inert names, no escape complications.
		jl.Message = extractJSONString(line, "MESSAGE")
		jl.Timestamp = extractJSONString(line, "__REALTIME_TIMESTAMP")
		jl.Priority = extractJSONString(line, "PRIORITY")
		out = append(out, jl)
	}
	return out
}

// extractJSONString pulls the value of "key": "..." from a flat
// JSON object string. Returns "" if not present. Cheap-and-robust:
// if the key isn't there, or the value isn't a string, return "".
// Real JSON parsing is overkill given the journalctl output shape.
func extractJSONString(s, key string) string {
	needle := `"` + key + `":"`
	i := strings.Index(s, needle)
	if i < 0 {
		return ""
	}
	rest := s[i+len(needle):]
	// Walk to the closing quote, handling \" inside the value.
	for j := 0; j < len(rest); j++ {
		if rest[j] == '\\' {
			j++ // skip the next char (it's escaped)
			continue
		}
		if rest[j] == '"' {
			return rest[:j]
		}
	}
	return ""
}

// Result body shapes — stable per the v1 schema commitment.

type journalLine struct {
	Timestamp string `json:"timestamp,omitempty"`
	Priority  string `json:"priority,omitempty"`
	Message   string `json:"message,omitempty"`
	// Raw preserves the entire journalctl JSON object for consumers
	// that need the full systemd metadata (UNIT, _PID, _UID, ...).
	Raw string `json:"raw,omitempty"`
}

type logsBody struct {
	Unit  string        `json:"unit"`
	Lines []journalLine `json:"lines"`
}

// WriteText renders the captured journal entries as human-readable text to w.
func (b logsBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if len(b.Lines) == 0 {
		fmt.Fprintf(bw, "no journal entries for %s\n", b.Unit)
	} else {
		fmt.Fprintf(bw, "%d entries from %s\n", len(b.Lines), b.Unit)
		for _, l := range b.Lines {
			fmt.Fprintf(bw, "  %s [%s] %s\n", l.Timestamp, l.Priority, l.Message)
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
