// Package markdown is an output.Renderer that emits CommonMark.
//
// Useful for piping CLI output into a wiki, a runbook PDF, or an
// incident review document. Unlike text/json/ndjson which target
// machines or terminals, markdown targets humans reading prose.
//
// Result rendering shape:
//
//	# pg_hardstorage <command>
//
//	*generated 2026-04-28T12:00:00Z*
//
//	## Status
//
//	(error block when the result is an error; otherwise omitted)
//
//	## Result
//
//	```json
//	{ ... pretty-printed JSON of result.body ... }
//	```
//
// Event rendering shape:
//
//   - **WARNING** `wal.stream/lag_high` · deployment=db1 · timeline=3
//     > suggestion text (when present)
//     ```json
//     { ... body ... }
//     ```
//
// The shape is plain CommonMark with code-fenced JSON for the
// structured payload. No HTML, no Markdown extensions — works in
// every common renderer (GitHub, GitLab, Confluence, Bear, Obsidian,
// pandoc).
package markdown

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Renderer is the markdown emitter.
type Renderer struct{}

// New returns a Renderer.
func New() *Renderer { return &Renderer{} }

// Name implements output.Renderer.
func (r *Renderer) Name() string { return "markdown" }

// SupportsTTY implements output.Renderer. Markdown isn't TTY-friendly
// in the absence of a renderer; the auto-detection prefers `text` on
// terminals and `json` off-terminal.
func (r *Renderer) SupportsTTY() bool { return false }

// Close implements output.Renderer.
func (r *Renderer) Close() error { return nil }

// RenderResult implements output.Renderer.
func (r *Renderer) RenderResult(w io.Writer, res *output.Result) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "# %s\n\n", res.Command)
	fmt.Fprintf(bw, "*generated %s*\n\n", res.GeneratedAt.UTC().Format(time.RFC3339))

	if res.IsError() {
		fmt.Fprintln(bw, "## Status")
		fmt.Fprintln(bw, "")
		fmt.Fprintf(bw, "**ERROR** — `%s`\n\n", res.Error.Code)
		fmt.Fprintf(bw, "> %s\n", res.Error.Message)
		if res.Error.Suggestion != nil {
			if res.Error.Suggestion.Human != "" {
				fmt.Fprintf(bw, ">\n> 💡 %s\n", res.Error.Suggestion.Human)
			}
			if res.Error.Suggestion.Command != "" {
				fmt.Fprintf(bw, ">\n> ```sh\n> %s\n> ```\n", res.Error.Suggestion.Command)
			}
			if res.Error.Suggestion.DocURL != "" {
				fmt.Fprintf(bw, ">\n> [docs](%s)\n", res.Error.Suggestion.DocURL)
			}
		}
		fmt.Fprintln(bw, "")
	}

	if res.Result != nil {
		fmt.Fprintln(bw, "## Result")
		fmt.Fprintln(bw, "")
		if err := writeFencedJSON(bw, res.Result); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

// RenderEvent implements output.Renderer. One bullet per event.
func (r *Renderer) RenderEvent(w io.Writer, ev *output.Event) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "- **%s** `%s/%s`",
		strings.ToUpper(ev.SeverityName), ev.Component, ev.Op)

	subj := renderSubject(ev.Subject)
	if subj != "" {
		fmt.Fprintf(bw, " · %s", subj)
	}
	fmt.Fprintln(bw)

	if ev.Suggestion != nil && ev.Suggestion.Human != "" {
		fmt.Fprintf(bw, "  > 💡 %s\n", ev.Suggestion.Human)
	}
	if ev.Body != nil {
		// Indent the fenced block so it nests under the bullet
		// point (CommonMark spec: 4-space indent for nested code).
		buf := &strings.Builder{}
		if err := writeFencedJSON(buf, ev.Body); err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			fmt.Fprintf(bw, "    %s\n", line)
		}
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

// renderSubject formats the Subject as a compact " · "-joined list.
// Same shape the slack renderer uses; keeps operator-readable
// content consistent across humans-vs-machine surfaces.
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

// writeFencedJSON pretty-prints body inside a ```json fenced block.
func writeFencedJSON(w io.Writer, body any) error {
	bs, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return fmt.Errorf("markdown: marshal body: %w", err)
	}
	if _, err := io.WriteString(w, "```json\n"); err != nil {
		return err
	}
	if _, err := w.Write(bs); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n```\n")
	return err
}
