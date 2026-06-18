// logs.go — CLI surface for journald log retrieval for the agent unit.
package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

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
                     RFC3339); passed verbatim to journalctl
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
		"start at this point (24h / yesterday / RFC3339); passed to journalctl")
	c.Flags().StringVar(&unit, "unit", "",
		"override the auto-derived systemd unit name")
	return c
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
		args = append(args, "--since", since)
	}

	// Mode A: the operator wants tail-style streaming output. We
	// exec journalctl with stdout/stderr inherited so the log
	// stream goes straight to their terminal. The dispatcher's
	// Result/Event mechanism doesn't suit a 24-hour-tail use case.
	if follow || d.Renderer().Name() == "text" {
		c := exec.CommandContext(cmd.Context(), bin, args...)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			// Exit code 1 from journalctl typically means "no entries
			// for this unit" — treat as a structured notfound rather
			// than a generic error so monitoring tools can pivot.
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				return output.NewError("notfound.unit",
					fmt.Sprintf("logs: no journal entries for unit %q (is the agent running?)",
						unit))
			}
			return fmt.Errorf("logs: journalctl: %w", err)
		}
		return nil
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
			return output.NewError("notfound.unit",
				fmt.Sprintf("logs: no journal entries for unit %q", unit))
		}
		return fmt.Errorf("logs: journalctl: %w", err)
	}
	body := logsBody{Unit: unit, Lines: parseJournalJSON(string(out))}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
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
