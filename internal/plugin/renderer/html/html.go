// Package html is an output.Renderer that emits self-contained HTML.
//
// Useful when an operator wants to ship a Result / Event into a wiki,
// Confluence page, or a status-report PDF (via headless-browser print)
// without first running it through a Markdown→HTML toolchain. The
// markdown renderer covers the Markdown case; this renderer covers
// every consumer that wants HTML directly.
//
// Output shape mirrors the markdown renderer's structure:
//
//	<!DOCTYPE html>
//	<html>
//	<head>
//	  <meta charset="utf-8">
//	  <title>pg_hardstorage <command></title>
//	  <style>...minimal CSS...</style>
//	</head>
//	<body>
//	  <h1>pg_hardstorage <command></h1>
//	  <p class="meta">generated 2026-04-29T...</p>
//	  <h2>Status</h2>
//	  (error block when result is an error; otherwise omitted)
//	  <h2>Result</h2>
//	  <pre><code class="json">...pretty JSON...</code></pre>
//	</body>
//	</html>
//
// Design choices:
//
//   - Self-contained: inline CSS, no external resources. The output
//     opens correctly in a wiki paste, an email, or a browser tab
//     without depending on stylesheets the consumer may not have.
//   - Conservative HTML: no JavaScript, no embedded SVG, no remote
//     references. The output is safe to paste into any HTML-rendering
//     surface that won't strip <style>.
//   - Escaping is paranoid: every operator-controlled value (message,
//     command, body content) goes through html.EscapeString so an
//     event with `<script>` in a body field can't escape its container.
package html

import (
	"encoding/json"
	"fmt"
	stdhtml "html"
	"io"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Renderer is the html emitter.
type Renderer struct{}

// New returns a Renderer.
func New() *Renderer { return &Renderer{} }

// Name implements output.Renderer.
func (r *Renderer) Name() string { return "html" }

// SupportsTTY implements output.Renderer. HTML in a TTY is illegible;
// the auto-detection picks `text` on terminals and `json` off-terminal,
// so html is always opt-in via --output html.
func (r *Renderer) SupportsTTY() bool { return false }

// Close implements output.Renderer.
func (r *Renderer) Close() error { return nil }

// RenderResult implements output.Renderer.
func (r *Renderer) RenderResult(w io.Writer, res *output.Result) error {
	bw := &strings.Builder{}
	bw.WriteString(htmlPreamble(res.Command))
	fmt.Fprintf(bw, "<h1>%s</h1>\n", stdhtml.EscapeString(res.Command))
	fmt.Fprintf(bw, "<p class=\"meta\">generated %s</p>\n",
		stdhtml.EscapeString(res.GeneratedAt.UTC().Format(time.RFC3339)))

	if res.IsError() {
		bw.WriteString("<h2>Status</h2>\n")
		fmt.Fprintf(bw, "<p class=\"error\"><strong>ERROR</strong> &mdash; <code>%s</code></p>\n",
			stdhtml.EscapeString(res.Error.Code))
		fmt.Fprintf(bw, "<blockquote>%s</blockquote>\n",
			stdhtml.EscapeString(res.Error.Message))
		if res.Error.Suggestion != nil {
			s := res.Error.Suggestion
			bw.WriteString("<aside class=\"suggestion\">\n")
			if s.Human != "" {
				fmt.Fprintf(bw, "<p>%s %s</p>\n",
					stdhtml.EscapeString("\U0001F4A1"), // 💡
					stdhtml.EscapeString(s.Human))
			}
			if s.Command != "" {
				fmt.Fprintf(bw, "<pre class=\"sh\"><code>%s</code></pre>\n",
					stdhtml.EscapeString(s.Command))
			}
			if s.DocURL != "" {
				// Defensive against javascript: / data: / file: scheme
				// abuse — only http(s) URLs land in href; anything
				// else is rendered as escaped text.
				if isSafeHTTPURL(s.DocURL) {
					fmt.Fprintf(bw, "<p><a href=\"%s\">docs</a></p>\n",
						stdhtml.EscapeString(s.DocURL))
				} else {
					fmt.Fprintf(bw, "<p>docs: <code>%s</code></p>\n",
						stdhtml.EscapeString(s.DocURL))
				}
			}
			bw.WriteString("</aside>\n")
		}
	}

	if res.Result != nil {
		bw.WriteString("<h2>Result</h2>\n")
		if err := writePreJSON(bw, res.Result); err != nil {
			return err
		}
	}
	bw.WriteString(htmlPostamble())
	_, err := io.WriteString(w, bw.String())
	return err
}

// RenderEvent implements output.Renderer. One <li> per event so a
// stream of events composes naturally inside an enclosing <ul>.
//
// Note: this method does NOT emit the surrounding <ul> tags — the
// caller composes them. Same posture as the markdown renderer's
// "- ..." bullet shape: the bullet is the unit, the list is the
// caller's concern.
func (r *Renderer) RenderEvent(w io.Writer, ev *output.Event) error {
	bw := &strings.Builder{}
	bw.WriteString("<li class=\"event\">\n")
	fmt.Fprintf(bw, "<strong>%s</strong> <code>%s/%s</code>",
		stdhtml.EscapeString(strings.ToUpper(ev.SeverityName)),
		stdhtml.EscapeString(ev.Component),
		stdhtml.EscapeString(ev.Op))

	subj := renderSubject(ev.Subject)
	if subj != "" {
		fmt.Fprintf(bw, " &middot; %s", stdhtml.EscapeString(subj))
	}
	bw.WriteString("\n")

	if ev.Suggestion != nil && ev.Suggestion.Human != "" {
		fmt.Fprintf(bw, "<blockquote>%s %s</blockquote>\n",
			stdhtml.EscapeString("\U0001F4A1"),
			stdhtml.EscapeString(ev.Suggestion.Human))
	}
	if ev.Body != nil {
		if err := writePreJSON(bw, ev.Body); err != nil {
			return err
		}
	}
	bw.WriteString("</li>\n")
	_, err := io.WriteString(w, bw.String())
	return err
}

// renderSubject produces the same compact " · "-joined shape the
// markdown renderer uses; consistent operator-readable form across
// surfaces.
func renderSubject(s output.Subject) string {
	parts := []string{}
	if s.Deployment != "" {
		parts = append(parts, "deployment="+s.Deployment)
	}
	if s.Tenant != "" && s.Tenant != "default" {
		parts = append(parts, "tenant="+s.Tenant)
	}
	if s.BackupID != "" {
		parts = append(parts, "backup="+s.BackupID)
	}
	if s.Timeline != 0 {
		parts = append(parts, fmt.Sprintf("timeline=%d", s.Timeline))
	}
	if s.LSN != "" {
		parts = append(parts, "lsn="+s.LSN)
	}
	return strings.Join(parts, " · ")
}

// writePreJSON pretty-prints body inside <pre><code class="json">...
// Every byte from the JSON encoder gets HTML-escaped to defend against
// a body field containing < / > / & / " / '.
func writePreJSON(w io.Writer, body any) error {
	bs, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return fmt.Errorf("html: marshal body: %w", err)
	}
	if _, err := io.WriteString(w, "<pre><code class=\"json\">"); err != nil {
		return err
	}
	if _, err := io.WriteString(w, stdhtml.EscapeString(string(bs))); err != nil {
		return err
	}
	_, err = io.WriteString(w, "</code></pre>\n")
	return err
}

// isSafeHTTPURL returns true iff s starts with http:// or https://.
// Used to gate href rendering; everything else falls back to
// rendering the URL as escaped text inside <code>.
func isSafeHTTPURL(s string) bool {
	low := strings.ToLower(s)
	return strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://")
}

// htmlPreamble returns the HTML <!DOCTYPE> + <head> + opening <body>.
// Inline CSS keeps the output self-contained.
func htmlPreamble(title string) string {
	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>` + stdhtml.EscapeString(title) + `</title>
<style>
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; max-width: 56rem; margin: 2rem auto; padding: 0 1rem; color: #222; }
h1 { border-bottom: 2px solid #ddd; padding-bottom: .25rem; }
h2 { margin-top: 1.5rem; color: #444; }
.meta { color: #888; font-size: .9em; }
.error { color: #b00; }
.suggestion { background: #fffbe6; border-left: 4px solid #fa0; padding: .5rem 1rem; margin: 1rem 0; }
blockquote { border-left: 4px solid #ccc; margin: .5rem 0; padding-left: 1rem; color: #555; }
pre { background: #f6f6f6; padding: .75rem; border-radius: 4px; overflow-x: auto; font-size: .9em; }
code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.event { margin-bottom: 1rem; }
.event blockquote { margin-left: 0; }
</style>
</head>
<body>
`
}

func htmlPostamble() string {
	return "</body>\n</html>\n"
}
