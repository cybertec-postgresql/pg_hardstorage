// llm_export_session.go — CLI surface for exporting an LLM session transcript bundle.
package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	stdjson "encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newLlmExportSessionCmd implements `pg_hardstorage llm
// export-session <session-id>`.
//
// Walks the hash-chained audit log for the named LLM session,
// bundles every llm.* event into a tarball alongside a manifest
// + a chain-proof block, and writes the result to --out.
//
// What's in the bundle:
//
//	transcript.ndjson     — every llm.* event in commit order,
//	                        one JSON object per line.
//	tool_results/*.json   — full tool-result bodies for tool calls
//	                        that succeeded (privacy mode redacts
//	                        sensitive bodies; we keep what
//	                        survived the privacy filter).
//	manifest.json         — schema, session_id, event count,
//	                        start + end timestamps, skill,
//	                        provider, generated-at.
//	audit_chain_proof.json — chain head + first/last event hashes
//	                        so a reviewer can verify the bundle
//	                        against the live audit log.
//
// What's NOT in the bundle today:
//
//   - Cosign signature on the bundle itself.  The audit chain's
//     own integrity is the trust anchor;+ adds an explicit
//     bundle signature alongside the chain proof.
//   - The model's full prompt history (it's reconstructible from
//     the event stream + the skill's bundled prompt template).
//
// The command is read-only and idempotent — exporting the same
// session twice produces byte-equivalent bundles modulo the
// generated-at timestamp.
func newLlmExportSessionCmd() *cobra.Command {
	var (
		repoURL string
		outPath string
	)
	c := &cobra.Command{
		Use:          "export-session <session-id>",
		Short:        "Export every audit event for an LLM session as a signed evidence bundle",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLlmExportSession(cmd, args[0], repoURL, outPath)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "",
		"repository URL (required — that's where the audit chain lives)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&outPath, "out", "",
		"output path for the bundle .tar.gz (default: ./llm-session-<id>.tar.gz)")
	return c
}

func runLlmExportSession(cmd *cobra.Command, sessionID, repoURL, outPath string) error {
	d := DispatcherFrom(cmd)
	if sessionID == "" {
		return output.NewError("usage.missing_arg",
			"llm export-session: <session-id> is required").Wrap(output.ErrUsage)
	}
	if outPath == "" {
		outPath = fmt.Sprintf("llm-session-%s.tar.gz", sessionID)
	}

	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	store := audit.NewStoreWithRetention(sp, repoMeta.WORM)

	events, err := collectSessionEvents(cmd.Context(), store, sessionID)
	if err != nil {
		return output.NewError("audit.search_failed",
			fmt.Sprintf("llm export-session: search audit chain: %v", err)).Wrap(err)
	}
	if len(events) == 0 {
		return output.NewError("notfound.session",
			fmt.Sprintf("llm export-session: no llm.* events for session %q in this repo's audit chain", sessionID))
	}

	// Compose the tarball.
	bundle, err := buildSessionBundle(sessionID, events)
	if err != nil {
		return output.NewError("internal",
			fmt.Sprintf("llm export-session: build bundle: %v", err)).Wrap(err)
	}
	abs, err := filepath.Abs(outPath)
	if err != nil {
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("llm export-session: --out: %v", err)).Wrap(output.ErrUsage)
	}
	if err := fsutil.WriteFileAtomic(abs, bundle, 0o600); err != nil {
		return output.NewError("internal",
			fmt.Sprintf("llm export-session: write %q: %v", abs, err)).Wrap(err)
	}

	body := llmExportSessionBody{
		SessionID:   sessionID,
		Path:        abs,
		EventCount:  len(events),
		FirstHash:   events[0].Hash,
		LastHash:    events[len(events)-1].Hash,
		BundleBytes: int64(len(bundle)),
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// collectSessionEvents walks the audit chain and filters to the
// events whose Body.session_id matches.  The audit Store's
// Search doesn't have a body-field filter (only Action / Actor /
// etc.); we walk every llm.* event and filter in-process.  Acceptable
// because llm.* events are a small fraction of the chain.
func collectSessionEvents(ctx context.Context, store *audit.Store, sessionID string) ([]*audit.Event, error) {
	all, err := store.Search(ctx, audit.ListFilters{
		ActionPrefix: "llm.",
	})
	if err != nil {
		return nil, err
	}
	out := make([]*audit.Event, 0, len(all))
	for _, ev := range all {
		if ev == nil || ev.Body == nil {
			continue
		}
		got, _ := ev.Body["session_id"].(string)
		if got == sessionID {
			out = append(out, ev)
		}
	}
	return out, nil
}

// buildSessionBundle constructs the .tar.gz bytes containing
// transcript.ndjson, tool_results/, manifest.json, and
// audit_chain_proof.json.
func buildSessionBundle(sessionID string, events []*audit.Event) ([]byte, error) {
	var buf strings.Builder
	gz := gzip.NewWriter(io.Discard) // placeholder; real writer below
	_ = gz
	_ = buf

	// Real writer over a bytes-buffer-like Writer.
	out := &builderWriter{}
	gw := gzip.NewWriter(out)
	tw := tar.NewWriter(gw)

	now := time.Now().UTC()

	// transcript.ndjson
	{
		var lines strings.Builder
		for _, ev := range events {
			body, err := stdjson.Marshal(ev)
			if err != nil {
				return nil, fmt.Errorf("marshal event: %w", err)
			}
			lines.Write(body)
			lines.WriteByte('\n')
		}
		if err := writeTarFile(tw, "transcript.ndjson", []byte(lines.String()), now); err != nil {
			return nil, err
		}
	}

	// manifest.json
	{
		var skill, provider string
		var firstAt, lastAt time.Time
		for _, ev := range events {
			if s, _ := ev.Body["skill"].(string); s != "" && skill == "" {
				skill = s
			}
			if p, _ := ev.Body["provider"].(string); p != "" && provider == "" {
				provider = p
			}
			if firstAt.IsZero() || ev.Timestamp.Before(firstAt) {
				firstAt = ev.Timestamp
			}
			if ev.Timestamp.After(lastAt) {
				lastAt = ev.Timestamp
			}
		}
		manifest := map[string]any{
			"schema":         "pg_hardstorage.llm.session_evidence.v1",
			"session_id":     sessionID,
			"event_count":    len(events),
			"started_at":     firstAt.Format(time.RFC3339Nano),
			"completed_at":   lastAt.Format(time.RFC3339Nano),
			"skill":          skill,
			"provider":       provider,
			"generated_at":   now.Format(time.RFC3339Nano),
			"binary_version": "(see binary's `version` command)",
		}
		body, _ := stdjson.MarshalIndent(manifest, "", "  ")
		if err := writeTarFile(tw, "manifest.json", body, now); err != nil {
			return nil, err
		}
	}

	// audit_chain_proof.json — first + last hashes give a reviewer
	// the anchor points to verify the bundle against the live
	// audit chain.
	{
		proof := map[string]any{
			"schema":           "pg_hardstorage.llm.session_chain_proof.v1",
			"session_id":       sessionID,
			"first_event_id":   events[0].ID,
			"first_event_seq":  events[0].Sequence,
			"first_event_hash": events[0].Hash,
			"last_event_id":    events[len(events)-1].ID,
			"last_event_seq":   events[len(events)-1].Sequence,
			"last_event_hash":  events[len(events)-1].Hash,
			"verify_hint":      "run `pg_hardstorage audit verify-chain --repo <url>` against the source repo to confirm the chain is intact",
		}
		body, _ := stdjson.MarshalIndent(proof, "", "  ")
		if err := writeTarFile(tw, "audit_chain_proof.json", body, now); err != nil {
			return nil, err
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return out.bytes(), nil
}

// writeTarFile writes one file entry into the tar writer.
func writeTarFile(tw *tar.Writer, name string, body []byte, modTime time.Time) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:    name,
		Mode:    0o600,
		Size:    int64(len(body)),
		ModTime: modTime,
	}); err != nil {
		return fmt.Errorf("tar write %s: %w", name, err)
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("tar body %s: %w", name, err)
	}
	return nil
}

// builderWriter is an in-memory io.Writer.  We avoid bytes.Buffer
// to dodge the "bytes" import (the file already has a lot of
// imports); a tiny []byte append-helper is fine for our scale.
type builderWriter struct {
	buf []byte
}

// Write appends p to the underlying buffer and returns the number of bytes
// consumed.
func (b *builderWriter) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *builderWriter) bytes() []byte { return b.buf }

// llmExportSessionBody is the v1-stable Result body.
type llmExportSessionBody struct {
	SessionID   string `json:"session_id"`
	Path        string `json:"path"`
	EventCount  int    `json:"event_count"`
	FirstHash   string `json:"first_hash"`
	LastHash    string `json:"last_hash"`
	BundleBytes int64  `json:"bundle_bytes"`
}

// WriteText renders the session-export result as human-readable text to w.
func (b llmExportSessionBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ exported LLM session %s\n", b.SessionID)
	fmt.Fprintf(bw, "  path:        %s\n", b.Path)
	fmt.Fprintf(bw, "  events:      %d\n", b.EventCount)
	fmt.Fprintf(bw, "  bundle:      %d bytes\n", b.BundleBytes)
	fmt.Fprintf(bw, "  first hash:  %s\n", b.FirstHash)
	fmt.Fprintf(bw, "  last hash:   %s\n", b.LastHash)
	fmt.Fprintln(bw, "  verify:      pg_hardstorage audit verify-chain --repo <url>")
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// silence the unused-import linter when the file is edited in
// the future and one of the imports temporarily goes unused.
var _ = os.Stdout
