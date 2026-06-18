package html_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/html"
)

func TestHTML_RenderResult_HappyPath(t *testing.T) {
	r := html.New()
	res := output.NewResult("status").WithBody(map[string]any{
		"deployment": "db1",
		"healthy":    true,
	})
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{
		"<!DOCTYPE html>",
		"<title>status</title>",
		"<h1>status</h1>",
		`<p class="meta">generated `,
		"<h2>Result</h2>",
		`<pre><code class="json">`,
		`&#34;deployment&#34;: &#34;db1&#34;`, // " escaped
		"</body>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestHTML_RenderResult_ErrorBlock(t *testing.T) {
	r := html.New()
	res := output.NewResult("x").WithError(output.NewError("wal.slot_missing",
		"Replication slot 'pg_hardstorage_db1' is not present").
		WithSuggestion(&output.Suggestion{
			Human:   "Recreate it",
			Command: "pg_hardstorage wal repair db1",
			DocURL:  "https://docs/runbooks/wal-slot-missing",
		}))
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{
		"<h2>Status</h2>",
		`<p class="error"><strong>ERROR</strong>`,
		`<code>wal.slot_missing</code>`,
		"<blockquote>Replication slot",
		`class="suggestion"`,
		"Recreate it",
		"pg_hardstorage wal repair db1",
		`<a href="https://docs/runbooks/wal-slot-missing">docs</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// HTML output gets pasted into wikis, emails, status pages — every
// operator-controlled byte must be safe. Plant a poison value in a
// body field AND in the command name (which goes through stdhtml.
// EscapeString directly, not via the JSON marshaler), and confirm no
// literal `<script>` survives.
//
// Two paths to verify:
//   - command/error message → html.EscapeString → `&lt;script&gt;`
//   - body fields           → JSON-marshalled (Go's HTML-safe default
//     emits `<script>` for `<`)
//
// Both shapes are safe inside `<pre>` / `<h1>` / etc.; what matters
// is that no literal `<script>` reaches the consumer.
func TestHTML_EscapesScriptInjections(t *testing.T) {
	r := html.New()
	const poison = `<script>alert("xss")</script>`
	// Plant in the command name to exercise the html.EscapeString path.
	res := output.NewResult(poison).WithBody(map[string]any{
		"deployment": poison,
	})
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if strings.Contains(got, "<script>") {
		t.Errorf("unescaped <script> tag survived:\n%s", got)
	}
	// The command-name path uses html.EscapeString → `&lt;`.
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Errorf("expected &lt;script&gt; from html.EscapeString path:\n%s", got)
	}
	// The body-field path goes through Go's JSON marshaler whose
	// HTML-safe default Unicode-escapes `<`, `>`, `&` — the
	// literal poison string never appears as raw bytes in the
	// output, satisfying the safety invariant. The
	// no-literal-<script> check above is the machine-checkable
	// version of that invariant.
}

// A non-http(s) DocURL must NOT land in href — defend against
// `javascript:` / `data:` / `file:` scheme abuse if a future event
// source carries operator-untrusted suggestion URLs.
func TestHTML_RefusesUnsafeDocURLScheme(t *testing.T) {
	r := html.New()
	res := output.NewResult("x").WithError(output.NewError("a", "b").
		WithSuggestion(&output.Suggestion{
			DocURL: `javascript:alert(1)`,
		}))
	var buf bytes.Buffer
	if err := r.RenderResult(&buf, res); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if strings.Contains(got, `<a href="javascript:`) {
		t.Errorf("javascript: scheme leaked into href:\n%s", got)
	}
	// Should be rendered as escaped text instead.
	if !strings.Contains(got, "<code>javascript:alert(1)</code>") {
		t.Errorf("unsafe URL should fall back to <code>; got:\n%s", got)
	}
}

func TestHTML_RenderEvent_ListItem(t *testing.T) {
	r := html.New()
	ev := output.NewEvent(output.SeverityWarning, "wal.stream", "lag_high").
		WithSubject(output.Subject{Deployment: "db1", Timeline: 3}).
		WithBody(map[string]any{"lag_seconds": 47})
	var buf bytes.Buffer
	if err := r.RenderEvent(&buf, ev); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{
		`<li class="event">`,
		"<strong>WARNING</strong>",
		"<code>wal.stream/lag_high</code>",
		"deployment=db1",
		"timeline=3",
		`<pre><code class="json">`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.HasPrefix(got, "<li class=") {
		t.Errorf("event should start with <li class=; got prefix: %q",
			firstChars(got, 30))
	}
}

func TestHTML_RendererMetadata(t *testing.T) {
	r := html.New()
	if r.Name() != "html" {
		t.Errorf("Name = %q", r.Name())
	}
	if r.SupportsTTY() {
		t.Error("html is not TTY-friendly")
	}
}

func firstChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
