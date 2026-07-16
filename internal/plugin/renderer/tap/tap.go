// Package tap is an output.Renderer that emits TAP version 14
// (Test Anything Protocol).  Useful for piping `pg_hardstorage`
// verify-namespace commands through a CI test harness:
//
//	pg_hardstorage integrity verify <id> -o tap | prove -e cat -
//	pg_hardstorage threshold attest verify <kind> <id> -o tap
//	pg_hardstorage replicate verify --from a --to b -o tap
//
// Closes the SPEC's renderer-inventory commitment for
// "verifier-as-test-harness" (alongside JUnit).  TAP 14 is the
// modern revision (https://testanything.org/tap-version-14-specification.html);
// it is backward-compatible with TAP 12/13 consumers (prove, jenkins
// TAP plugin, GitHub TAP renderers).
//
// Strategy:
//
//   - Result with no error: walk the body for "test points".  If the
//     body has a `findings` / `failures` slice, each entry becomes one
//     test point (failure → not ok).  Otherwise the body becomes one
//     test point that passes if the result has no error.
//   - Result with error: one not-ok line carrying the structured
//     error code + message + suggestion, with a YAML diagnostic
//     block.
//   - Streaming Event: one TAP line per event; severity ≤ warning →
//     ok, severity ≥ error → not ok.
//
// We deliberately don't try to invent test "directives" (SKIP, TODO)
// from the body — those are testkit-specific signals and would
// surprise non-testkit consumers.
package tap

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/jsonshape"
)

// Renderer emits TAP 14.  Stateless.
type Renderer struct{}

// New returns a Renderer.
func New() *Renderer { return &Renderer{} }

// Name implements output.Renderer.
func (r *Renderer) Name() string { return "tap" }

// SupportsTTY implements output.Renderer.
func (r *Renderer) SupportsTTY() bool { return false }

// Close implements output.Renderer.
func (r *Renderer) Close() error { return nil }

// RenderResult walks the result body and emits TAP test points.
func (r *Renderer) RenderResult(w io.Writer, res *output.Result) error {
	bw := &strings.Builder{}
	fmt.Fprintln(bw, "TAP version 14")

	if res.IsError() {
		fmt.Fprintln(bw, "1..1")
		fmt.Fprintf(bw, "not ok 1 - %s\n", commandLabel(res.Command))
		writeYAMLBlock(bw, errorDiagnostic(res))
		_, err := io.WriteString(w, bw.String())
		return err
	}

	points := extractPoints(res)
	if len(points) == 0 {
		// No body, no findings — record one passing test point.
		fmt.Fprintln(bw, "1..1")
		fmt.Fprintf(bw, "ok 1 - %s\n", commandLabel(res.Command))
		_, err := io.WriteString(w, bw.String())
		return err
	}
	fmt.Fprintf(bw, "1..%d\n", len(points))
	for i, p := range points {
		num := i + 1
		if p.Failed {
			fmt.Fprintf(bw, "not ok %d - %s\n", num, p.Description)
			if p.Diagnostic != nil {
				writeYAMLBlock(bw, p.Diagnostic)
			}
		} else {
			fmt.Fprintf(bw, "ok %d - %s\n", num, p.Description)
		}
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

// RenderEvent emits one TAP line per streaming event.  We don't
// emit a plan line for events; consumers running TAP 14 in
// streaming mode get a "no plan" fallback that prove(1) handles.
func (r *Renderer) RenderEvent(w io.Writer, ev *output.Event) error {
	desc := strings.TrimSpace(ev.Op)
	if desc == "" {
		desc = ev.Component
	}
	failed := severityIsFailure(ev.Severity)
	verb := "ok"
	if failed {
		verb = "not ok"
	}
	// Streaming TAP doesn't carry a numbered plan; we use 1 as a
	// stable index per line so consumers can still parse it.  prove
	// + jenkins TAP plugin both accept this.
	if _, err := fmt.Fprintf(w, "%s 1 - [%s] %s\n", verb, ev.SeverityName, desc); err != nil {
		return err
	}
	if failed && ev.Body != nil {
		writeYAMLBlock(w, eventBodyToMap(ev))
	}
	return nil
}

// ----- helpers -----

// point is one TAP test point.
type point struct {
	Description string
	Failed      bool
	Diagnostic  map[string]any
}

// extractPoints derives test points from a result body via three
// heuristics, in order of preference:
//
//  1. Body has a top-level `failures` slice → one point per failure
//     (all failed).  Plus one summary "ok" point if `met = true`.
//  2. Body has a top-level `findings` slice → one point per finding,
//     each pass-or-fail driven by the finding's severity.
//  3. Otherwise: one point with the result's overall verdict.  We
//     look for `met`, `signature_valid`, `status` (ok / found_issues /
//     drifted / broken), `verdict`, in that order; if absent we
//     default to passing (the command exited 0).
//
// We round-trip through JSON so the renderer doesn't need per-
// command schema knowledge — every body uses the same JSON shape.
func extractPoints(res *output.Result) []point {
	if res.Result == nil {
		return nil
	}
	tree, err := jsonshape.RoundTrip(res.Result)
	if err != nil {
		return nil
	}
	v, ok := tree.(map[string]any)
	if !ok {
		return nil
	}
	var points []point

	// Rule 1: failures slice.
	if fails, ok := v["failures"].([]any); ok && len(fails) > 0 {
		for _, f := range fails {
			points = append(points, failureToPoint(res.Command, f))
		}
	}

	// Rule 2: findings slice (insider scans, etc.).
	if findings, ok := v["findings"].([]any); ok && len(findings) > 0 {
		for _, f := range findings {
			points = append(points, findingToPoint(res.Command, f))
		}
	}

	// If we already gathered findings/failures points, prepend a
	// summary line that captures the overall verdict.
	if len(points) > 0 {
		summary := summaryPoint(res.Command, v, len(points))
		// Renumber: summary goes first.
		points = append([]point{summary}, points...)
		return points
	}

	// Rule 3: overall verdict.
	return []point{summaryPoint(res.Command, v, 0)}
}

func summaryPoint(command string, body map[string]any, sub int) point {
	failed := false
	desc := commandLabel(command)
	if v := boolField(body, "met"); v != nil && !*v {
		failed = true
		desc = commandLabel(command) + " — quorum not met"
	} else if v := boolField(body, "signature_valid"); v != nil && !*v {
		failed = true
		desc = commandLabel(command) + " — signature invalid"
	}
	if status := strField(body, "status"); status != "" {
		switch strings.ToLower(status) {
		case "ok", "consistent", "valid", "verified":
			// pass
		case "found_issues", "drifted", "broken", "error", "invalid", "tampered":
			failed = true
			desc = commandLabel(command) + " — status " + status
		}
	}
	if v := strField(body, "verdict"); v != "" {
		desc = commandLabel(command) + " — verdict " + v
	}
	p := point{Description: desc, Failed: failed}
	if failed {
		p.Diagnostic = map[string]any{
			"command": command,
			"summary": body,
		}
	}
	if sub > 0 {
		p.Description = fmt.Sprintf("%s (%d sub-points follow)", desc, sub)
	}
	return p
}

func failureToPoint(command string, f any) point {
	m, _ := f.(map[string]any)
	desc := command + " failure"
	if reason := strField(m, "reason"); reason != "" {
		desc = reason
	}
	if hash := strField(m, "chunk_hash"); hash != "" {
		desc = "chunk " + shorten(hash, 12) + " — " + desc
	}
	if id := strField(m, "backup_id"); id != "" {
		desc = id + " — " + desc
	}
	return point{Description: desc, Failed: true, Diagnostic: m}
}

func findingToPoint(command string, f any) point {
	m, _ := f.(map[string]any)
	sev := strField(m, "severity")
	failed := severityNameIsFailure(sev)
	desc := command + " finding"
	if t := strField(m, "type"); t != "" {
		desc = t
	}
	if actor := strField(m, "actor"); actor != "" {
		desc = actor + " — " + desc
	}
	if reason := strField(m, "reason"); reason != "" {
		desc += ": " + reason
	}
	p := point{Description: desc, Failed: failed}
	if failed {
		p.Diagnostic = m
	}
	return p
}

func errorDiagnostic(res *output.Result) map[string]any {
	m := map[string]any{
		"command": res.Command,
		"code":    res.Error.Code,
		"message": res.Error.Message,
	}
	if res.Error.Suggestion != nil {
		s := map[string]any{}
		if res.Error.Suggestion.Human != "" {
			s["human"] = res.Error.Suggestion.Human
		}
		if res.Error.Suggestion.Command != "" {
			s["command"] = res.Error.Suggestion.Command
		}
		if res.Error.Suggestion.DocURL != "" {
			s["doc_url"] = res.Error.Suggestion.DocURL
		}
		if len(s) > 0 {
			m["suggestion"] = s
		}
	}
	return m
}

func eventBodyToMap(ev *output.Event) map[string]any {
	m := map[string]any{
		"component": ev.Component,
		"op":        ev.Op,
		"severity":  ev.SeverityName,
	}
	if ev.Body != nil {
		bs, err := json.Marshal(ev.Body)
		if err == nil {
			var b map[string]any
			if err := json.Unmarshal(bs, &b); err == nil {
				m["body"] = b
			}
		}
	}
	return m
}

// writeYAMLBlock writes a TAP YAML diagnostic block.  Keys sorted
// for deterministic output.  Nested maps/slices are indented two
// spaces under the key.
func writeYAMLBlock(w io.Writer, m map[string]any) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintln(w, "  ---")
	for _, k := range keys {
		writeYAMLField(w, "  ", k, m[k])
	}
	fmt.Fprintln(w, "  ...")
}

func writeYAMLField(w io.Writer, indent, key string, v any) {
	switch val := v.(type) {
	case map[string]any:
		fmt.Fprintf(w, "%s%s:\n", indent, key)
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			writeYAMLField(w, indent+"  ", k, val[k])
		}
	case []any:
		fmt.Fprintf(w, "%s%s:\n", indent, key)
		for _, item := range val {
			switch sub := item.(type) {
			case map[string]any:
				keys := make([]string, 0, len(sub))
				for k := range sub {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				first := true
				for _, k := range keys {
					prefix := indent + "  "
					if first {
						fmt.Fprintf(w, "%s- ", indent)
						prefix = indent + "  "
						writeYAMLFieldSameLine(w, prefix, k, sub[k])
						first = false
					} else {
						writeYAMLField(w, indent+"  ", k, sub[k])
					}
				}
			default:
				fmt.Fprintf(w, "%s- %s\n", indent, scalarYAML(item))
			}
		}
	default:
		fmt.Fprintf(w, "%s%s: %s\n", indent, key, scalarYAML(v))
	}
}

// writeYAMLFieldSameLine writes the first field of a list-of-maps on
// the same physical line as the leading "- ".  Subsequent fields are
// indented to the right of the dash.
func writeYAMLFieldSameLine(w io.Writer, indent, key string, v any) {
	switch v.(type) {
	case map[string]any, []any:
		// Complex value — print key on its own line, then the value
		// indented under it.
		fmt.Fprintf(w, "%s:\n", key)
		writeYAMLField(w, indent, key, v)
	default:
		fmt.Fprintf(w, "%s: %s\n", key, scalarYAML(v))
	}
}

func scalarYAML(v any) string {
	if v == nil {
		return "~"
	}
	switch val := v.(type) {
	case string:
		// Quote when the value would otherwise be misparsed.
		if needsYAMLQuote(val) {
			b, _ := json.Marshal(val)
			return string(b)
		}
		return val
	case bool, int, int64, float64:
		return fmt.Sprintf("%v", val)
	case map[string]any, []any:
		bs, err := json.Marshal(val)
		if err == nil {
			return string(bs)
		}
		return fmt.Sprintf("%v", val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

func needsYAMLQuote(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		switch r {
		case ':', '#', '"', '\'', '\n', '\r', '\t':
			return true
		}
	}
	switch strings.ToLower(s) {
	case "null", "true", "false", "yes", "no", "on", "off", "~":
		return true
	}
	return false
}

func severityIsFailure(sev output.Severity) bool {
	// RFC 5424 levels: emergency=0, alert=1, critical=2, error=3,
	// warning=4, notice=5, info=6, debug=7.  Anything ≤ error is a
	// failure.
	return sev <= output.SeverityError
}

func severityNameIsFailure(name string) bool {
	switch strings.ToLower(name) {
	case "emergency", "alert", "critical", "error", "fatal":
		return true
	case "warning", "warn":
		// Treat warning as a failure for the test-harness use case —
		// CI usually wants to know about warning-level findings.
		return true
	}
	return false
}

func commandLabel(command string) string {
	if command == "" {
		return "pg_hardstorage"
	}
	return command
}

func boolField(m map[string]any, key string) *bool {
	v, ok := m[key]
	if !ok {
		return nil
	}
	b, ok := v.(bool)
	if !ok {
		return nil
	}
	return &b
}

func truePtr(b bool) *bool { return &b }

func strField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
