// onboard_helpers.go — 'lint' + 'explain' + 'glossary' commands surfaced during onboarding.
package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/version"
)

// newLintCmdImpl validates the resolved pg_hardstorage.yaml (honouring
// -c/--config) using the SAME loader real commands use — strict
// KnownFields + validate() — instead of the previous stub that always
// returned {"status":"valid"} without reading anything.
func newLintCmdImpl() *cobra.Command {
	return &cobra.Command{
		Use: "lint", Short: "Validate pg_hardstorage.yaml",
		Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			p, err := paths.Resolve(paths.DefaultOptions())
			if err != nil {
				return output.NewError("internal", err.Error()).Wrap(err)
			}
			loaded, err := config.Load(p)
			if err != nil {
				// A parse/validation failure is the whole point of lint:
				// report it as invalid with the reason, and exit non-zero
				// so CI/pre-flight scripts catch a broken config.
				return output.NewError("config.invalid",
					fmt.Sprintf("lint: %v", err)).
					WithSuggestion(&output.Suggestion{
						Human: "fix the reported field/key and re-run; see the config reference for valid keys and schemes",
					}).Wrap(output.ErrUsage)
			}
			deployments := 0
			if loaded != nil {
				deployments = len(loaded.Config.Deployments)
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(lintBody{
				Status:      "valid",
				Deployments: deployments,
			}))
		},
	}
}

type lintBody struct {
	Status      string `json:"status"`
	Deployments int    `json:"deployments"`
}

func (b lintBody) WriteText(w io.Writer) error {
	_, err := fmt.Fprintf(w, "✓ config valid — %d deployment(s)\n", b.Deployments)
	return err
}

// newExplainCmdImpl returns the real help for a command path instead of
// echoing the argument back. `explain backup` now surfaces backup's
// Short/Long + usage; an unknown command is a usage error naming it.
func newExplainCmdImpl() *cobra.Command {
	return &cobra.Command{
		Use: "explain <cmd>", Short: "Explain pg_hardstorage commands",
		Args: cobra.MinimumNArgs(1), SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := DispatcherFrom(cmd)
			target, _, err := cmd.Root().Find(args)
			if err != nil || target == nil || target == cmd.Root() {
				return output.NewError("notfound.command",
					fmt.Sprintf("explain: no such command %q", strings.Join(args, " "))).
					WithSuggestion(&output.Suggestion{
						Human:   "run `pg_hardstorage --help` for the command list",
						Command: "pg_hardstorage --help",
					})
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(explainBody{
				Command:  target.CommandPath(),
				Summary:  target.Short,
				Details:  strings.TrimSpace(target.Long),
				Usage:    strings.TrimSpace(target.UseLine()),
				HelpHint: fmt.Sprintf("%s --help", target.CommandPath()),
			}))
		},
	}
}

type explainBody struct {
	Command  string `json:"command"`
	Summary  string `json:"summary"`
	Details  string `json:"details,omitempty"`
	Usage    string `json:"usage"`
	HelpHint string `json:"help_hint"`
}

func (b explainBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "%s — %s\n", b.Command, b.Summary)
	fmt.Fprintf(bw, "  usage: %s\n", b.Usage)
	if b.Details != "" {
		fmt.Fprintf(bw, "\n%s\n", b.Details)
	}
	fmt.Fprintf(bw, "\n(full flags: %s)\n", b.HelpHint)
	_, err := io.WriteString(w, bw.String())
	return err
}

func newChangelogCmdImpl() *cobra.Command {
	return &cobra.Command{
		Use: "changelog", Short: "Show changelog",
		Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(map[string]any{
				"version":   version.Version,
				"changelog": "https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/CHANGELOG.md",
			}))
		},
	}
}

// glossaryTerms is the canonical term list. `glossary` lists them all;
// `glossary <term>` looks one up (case-insensitive) and returns its
// definition — previously the lookup path threw the description away and
// echoed back just {"term": args[0]}.
var glossaryTerms = []glossaryEntry{
	{"deployment", "A PostgreSQL instance or cluster you back up, named in pg_hardstorage.yaml."},
	{"backup", "One PITR-recoverable artifact: a base backup plus the manifest that describes it."},
	{"repo", "A content-addressed repository (file/s3/gcs/azblob/sftp/scp) holding chunks, manifests, and WAL."},
	{"chunk", "A content-addressed, deduplicated, compressed (and optionally encrypted) unit of backup data."},
	{"manifest", "The signed description of a backup: its files, chunk references, and metadata."},
	{"wal", "Write-Ahead Log — the PostgreSQL change stream pg_hardstorage archives for point-in-time recovery."},
	{"pitr", "Point-In-Time Recovery: restoring to an arbitrary moment by replaying archived WAL over a base backup."},
	{"rpo", "Recovery Point Objective — how much recent data you can afford to lose (the age of your newest recoverable point)."},
	{"rto", "Recovery Time Objective — how long a restore is allowed to take."},
	{"kek", "Key-Encryption Key: the long-lived key that wraps each backup's data key (DEK)."},
	{"dek", "Data-Encryption Key: the per-backup key that encrypts chunk data; stored wrapped by the KEK."},
	{"tombstone", "A soft-delete marker: the manifest is retired but chunks survive until the next `repo gc --apply`."},
	{"hold", "A legal/retention hold that excludes a backup from rotation and deletion until released."},
	{"retention", "The policy (gfs/simple/count) that decides which backups `rotate` keeps or tombstones."},
	{"incremental", "A backup capturing only blocks changed since a parent (requires PostgreSQL 17+)."},
	{"timeline", "A PostgreSQL history branch (TLI); a promotion/recovery forks a new timeline."},
	{"lsn", "Log Sequence Number — a position in the WAL stream used as a PITR target."},
}

type glossaryEntry struct {
	Term        string `json:"term"`
	Description string `json:"description"`
}

func newGlossaryCmdImpl() *cobra.Command {
	return &cobra.Command{
		Use: "glossary [<term>]", Short: "Look up pg_hardstorage terminology",
		Args: cobra.MaximumNArgs(1), SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := DispatcherFrom(cmd)
			if len(args) == 0 {
				entries := append([]glossaryEntry(nil), glossaryTerms...)
				sort.Slice(entries, func(i, j int) bool { return entries[i].Term < entries[j].Term })
				return d.Result(output.NewResult(cmd.CommandPath()).WithBody(glossaryListBody{Entries: entries}))
			}
			q := strings.ToLower(strings.TrimSpace(args[0]))
			for _, e := range glossaryTerms {
				if strings.ToLower(e.Term) == q {
					return d.Result(output.NewResult(cmd.CommandPath()).WithBody(e))
				}
			}
			return output.NewError("notfound.term",
				fmt.Sprintf("glossary: no entry for %q", args[0])).
				WithSuggestion(&output.Suggestion{
					Human:   "run `pg_hardstorage glossary` (no argument) to list every term",
					Command: "pg_hardstorage glossary",
				})
		},
	}
}

type glossaryListBody struct {
	Entries []glossaryEntry `json:"entries"`
}

func (b glossaryListBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	for _, e := range b.Entries {
		fmt.Fprintf(bw, "  %-12s %s\n", e.Term, e.Description)
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

func (e glossaryEntry) WriteText(w io.Writer) error {
	_, err := fmt.Fprintf(w, "%s — %s\n", e.Term, e.Description)
	return err
}

// newDemoCmdImpl now lives in demo.go (issue #15) — it runs the real
// end-to-end flow instead of printing a placeholder message.
