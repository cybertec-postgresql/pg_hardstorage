// Package junit is an output.Renderer that emits JUnit XML — the
// `surefire` / `surefire-style` format Jenkins, GitLab CI, GitHub
// Actions, CircleCI and most other CI dashboards consume natively.
//
// Closes the SPEC's "verifier-as-test-harness" renderer
// commitment alongside the TAP renderer.  The two are
// complementary: TAP for shell-based prove(1) pipelines; JUnit XML
// for CI dashboards that show per-suite/per-case histograms.
//
// Strategy:
//
//   - Result with no error: walk the body for "test cases".
//   - If body has `failures` slice → one testcase per failure.
//   - If body has `findings` slice → one testcase per finding;
//     severity ≥ warning → wrapped in <failure>.
//   - Otherwise: one testcase carrying the overall verdict.
//   - Result with error: one testcase wrapped in <failure>
//     (transport-style errors that prevented the verify itself
//     would normally be <error>; we use <failure> for parity with
//     the way verify-namespace exit codes flow).
//   - Streaming Event: deferred — JUnit isn't a streaming format.
//     We render each event as one testcase in a single suite that
//     accumulates across the run (but the dispatcher invokes
//     RenderEvent per-event and writes are flushed individually,
//     so callers wanting batched JUnit-of-events should buffer
//     externally).  For now RenderEvent emits a single-suite XML
//     fragment per event so the bytes are at least parseable.
//
// Determinism: every map iteration is sorted before serialising;
// timestamps are written in RFC3339 UTC; element order is fixed
// (testsuites → testsuite → testcase → failure/error).
package junit

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Renderer emits JUnit XML.  Stateless.
type Renderer struct{}

// New returns a Renderer.
func New() *Renderer { return &Renderer{} }

// Name implements output.Renderer.
func (r *Renderer) Name() string { return "junit" }

// SupportsTTY implements output.Renderer.
func (r *Renderer) SupportsTTY() bool { return false }

// Close implements output.Renderer.
func (r *Renderer) Close() error { return nil }

// XML element types — exported so tests can decode + assert.
type TestSuites struct {
	XMLName  xml.Name    `xml:"testsuites"`
	Name     string      `xml:"name,attr,omitempty"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Errors   int         `xml:"errors,attr"`
	Time     string      `xml:"time,attr,omitempty"`
	Suites   []TestSuite `xml:"testsuite"`
}

// TestSuite is one <testsuite> entry: aggregate counters plus the
// individual cases. We emit one suite per render call.
type TestSuite struct {
	Name      string     `xml:"name,attr"`
	Tests     int        `xml:"tests,attr"`
	Failures  int        `xml:"failures,attr"`
	Errors    int        `xml:"errors,attr"`
	Skipped   int        `xml:"skipped,attr"`
	Time      string     `xml:"time,attr,omitempty"`
	Timestamp string     `xml:"timestamp,attr,omitempty"`
	Cases     []TestCase `xml:"testcase"`
}

// TestCase is a single <testcase> entry — one Event maps to one
// case. Failure / Error / Skipped are mutually exclusive; absence
// means the case passed.
type TestCase struct {
	Classname string   `xml:"classname,attr,omitempty"`
	Name      string   `xml:"name,attr"`
	Time      string   `xml:"time,attr,omitempty"`
	Failure   *Failure `xml:"failure,omitempty"`
	Error     *Error   `xml:"error,omitempty"`
	Skipped   *Skipped `xml:"skipped,omitempty"`
}

// Failure marks a testcase as failing.  Body is the structured
// detail (serialised as the element's character data).
type Failure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr,omitempty"`
	Body    string `xml:",chardata"`
}

// Error marks a testcase as having errored (couldn't run).
type Error struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr,omitempty"`
	Body    string `xml:",chardata"`
}

// Skipped marks a testcase as skipped.
type Skipped struct {
	Message string `xml:"message,attr,omitempty"`
}

// RenderResult produces a one-shot JUnit XML body.
func (r *Renderer) RenderResult(w io.Writer, res *output.Result) error {
	suites := buildSuitesFromResult(res)
	return writeXML(w, suites)
}

// RenderEvent produces a single-suite JUnit XML fragment per event.
// JUnit isn't a streaming format; this is best-effort so that
// `… -o junit` still works on streaming commands.
func (r *Renderer) RenderEvent(w io.Writer, ev *output.Event) error {
	failed := severityIsFailure(ev.SeverityName)
	tc := TestCase{
		Classname: ev.Component,
		Name:      ev.Op,
	}
	if failed {
		tc.Failure = &Failure{
			Message: fmt.Sprintf("[%s] %s", ev.SeverityName, ev.Op),
			Type:    ev.SeverityName,
			Body:    eventBodyToText(ev),
		}
	}
	suite := TestSuite{
		Name:      ev.Component,
		Tests:     1,
		Failures:  boolToInt(failed),
		Timestamp: ev.GeneratedAt.UTC().Format(time.RFC3339),
		Cases:     []TestCase{tc},
	}
	suites := TestSuites{
		Name:     "pg_hardstorage events",
		Tests:    1,
		Failures: boolToInt(failed),
		Suites:   []TestSuite{suite},
	}
	return writeXML(w, suites)
}

// ----- builders -----

func buildSuitesFromResult(res *output.Result) TestSuites {
	suite := TestSuite{
		Name:      res.Command,
		Timestamp: res.GeneratedAt.UTC().Format(time.RFC3339),
	}

	if res.IsError() {
		tc := TestCase{
			Classname: res.Command,
			Name:      "result",
			Failure: &Failure{
				Message: res.Error.Code + ": " + res.Error.Message,
				Type:    res.Error.Code,
				Body:    errorBodyText(res),
			},
		}
		suite.Cases = []TestCase{tc}
		suite.Tests = 1
		suite.Failures = 1
		return TestSuites{
			Name:     res.Command,
			Tests:    1,
			Failures: 1,
			Suites:   []TestSuite{suite},
		}
	}

	cases := buildCasesFromBody(res.Command, res.Result)
	if len(cases) == 0 {
		// Empty body — record one passing test case.
		cases = append(cases, TestCase{
			Classname: res.Command,
			Name:      "result",
		})
	}
	suite.Cases = cases
	suite.Tests = len(cases)
	for _, c := range cases {
		if c.Failure != nil {
			suite.Failures++
		}
		if c.Error != nil {
			suite.Errors++
		}
		if c.Skipped != nil {
			suite.Skipped++
		}
	}
	return TestSuites{
		Name:     res.Command,
		Tests:    suite.Tests,
		Failures: suite.Failures,
		Errors:   suite.Errors,
		Suites:   []TestSuite{suite},
	}
}

// buildCasesFromBody walks the body for findings/failures the same
// way the TAP renderer does.  Same heuristics; same output shape.
func buildCasesFromBody(command string, body any) []TestCase {
	if body == nil {
		return nil
	}
	bs, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal(bs, &v); err != nil {
		return nil
	}
	var cases []TestCase

	// Summary case: one testcase reflecting the overall verdict.
	cases = append(cases, summaryCase(command, v))

	// Per-failure cases.
	if fails, ok := v["failures"].([]any); ok {
		for _, f := range fails {
			cases = append(cases, failureToCase(command, f))
		}
	}
	// Per-finding cases.
	if findings, ok := v["findings"].([]any); ok {
		for _, f := range findings {
			cases = append(cases, findingToCase(command, f))
		}
	}
	return cases
}

func summaryCase(command string, body map[string]any) TestCase {
	failed := false
	name := "summary"
	if v := boolField(body, "met"); v != nil && !*v {
		failed = true
		name = "summary-quorum-not-met"
	}
	if v := boolField(body, "signature_valid"); v != nil && !*v {
		failed = true
		name = "summary-signature-invalid"
	}
	if status := strField(body, "status"); status != "" {
		switch strings.ToLower(status) {
		case "found_issues", "drifted", "broken", "error", "invalid", "tampered":
			failed = true
			name = "summary-status-" + sanitiseName(status)
		}
	}
	tc := TestCase{
		Classname: command,
		Name:      name,
	}
	if failed {
		tc.Failure = &Failure{
			Message: "summary failed: " + name,
			Type:    "summary",
			Body:    bodyAsText(body),
		}
	}
	return tc
}

func failureToCase(command string, f any) TestCase {
	m, _ := f.(map[string]any)
	name := "failure"
	if hash := strField(m, "chunk_hash"); hash != "" {
		name = "chunk-" + shorten(hash, 12)
	}
	if id := strField(m, "backup_id"); id != "" {
		name = "backup-" + id
	}
	reason := strField(m, "reason")
	tc := TestCase{
		Classname: command,
		Name:      name,
		Failure: &Failure{
			Message: reason,
			Type:    "failure",
			Body:    bodyAsText(m),
		},
	}
	return tc
}

func findingToCase(command string, f any) TestCase {
	m, _ := f.(map[string]any)
	sev := strField(m, "severity")
	failed := severityIsFailure(sev)
	name := "finding"
	if t := strField(m, "type"); t != "" {
		name = t
	}
	if actor := strField(m, "actor"); actor != "" {
		name = actor + "/" + name
	}
	tc := TestCase{
		Classname: command,
		Name:      sanitiseName(name),
	}
	if failed {
		tc.Failure = &Failure{
			Message: strField(m, "reason"),
			Type:    sev,
			Body:    bodyAsText(m),
		}
	}
	return tc
}

func errorBodyText(res *output.Result) string {
	parts := []string{
		"code: " + res.Error.Code,
		"message: " + res.Error.Message,
	}
	if res.Error.Suggestion != nil {
		if h := res.Error.Suggestion.Human; h != "" {
			parts = append(parts, "suggestion.human: "+h)
		}
		if c := res.Error.Suggestion.Command; c != "" {
			parts = append(parts, "suggestion.command: "+c)
		}
		if d := res.Error.Suggestion.DocURL; d != "" {
			parts = append(parts, "suggestion.doc_url: "+d)
		}
	}
	return strings.Join(parts, "\n")
}

func bodyAsText(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\n", k, scalarText(m[k]))
	}
	return strings.TrimRight(b.String(), "\n")
}

func eventBodyToText(ev *output.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "component: %s\n", ev.Component)
	fmt.Fprintf(&b, "op: %s\n", ev.Op)
	fmt.Fprintf(&b, "severity: %s\n", ev.SeverityName)
	if ev.Body != nil {
		bs, err := json.Marshal(ev.Body)
		if err == nil {
			fmt.Fprintf(&b, "body: %s", string(bs))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func scalarText(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case map[string]any, []any:
		bs, _ := json.Marshal(val)
		return string(bs)
	default:
		return fmt.Sprintf("%v", val)
	}
}

func writeXML(w io.Writer, suites TestSuites) error {
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(suites); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}
	return nil
}

// ----- shared helpers -----

func severityIsFailure(name string) bool {
	switch strings.ToLower(name) {
	case "emergency", "alert", "critical", "error", "fatal":
		return true
	case "warning", "warn":
		return true
	}
	return false
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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// sanitiseName makes a string safe for the JUnit name attribute.
// CI dashboards index by name; spaces/colons/slashes confuse the
// query interfaces.
func sanitiseName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '/':
			out = append(out, r)
		case r == ' ':
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "unnamed"
	}
	return string(out)
}
