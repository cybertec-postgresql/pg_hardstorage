// redact.go — CLI surface for applying and previewing PII redaction SQL.
package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/redact"
)

// newRedactCmd implements `pg_hardstorage redact apply` —
// post-restore PII redaction.
//
// The command reads a rules YAML, validates it, and runs the
// generated UPDATE statements against the target PostgreSQL
// cluster.  Typical workflow:
//
//	$ pg_hardstorage restore prod-2026 --target /var/lib/postgresql/staging
//	$ pg_ctl -D /var/lib/postgresql/staging start
//	$ pg_hardstorage redact apply \
//	      --rules /etc/pg_hardstorage/redact-prod.yaml \
//	      --pg-connection "postgres://postgres@localhost:5433/prod"
//
// The command shells out to psql (same posture as `db
// install-extension`) so the binary doesn't grow a libpq
// client dependency.  Each table's UPDATE runs inside its
// own transaction so a per-table failure leaves that table
// untouched while preceding tables stay redacted.
//
// Use --dry-run to print the SQL without applying.  Use
// --print-sql to emit the SQL on stdout for piping into a
// custom workflow.  Use --salt-hex to override the random
// salt for join-preserving redactions across runs.
func newRedactCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "redact <apply|preview>",
		Short: "Post-restore PII redaction (rules-driven UPDATE statements)",
	}
	c.AddCommand(newRedactApplyCmd(), newRedactPreviewCmd())
	return c
}

func newRedactApplyCmd() *cobra.Command {
	var (
		rulesPath string
		pgConn    string
		dryRun    bool
		printSQL  bool
		saltHex   string
	)
	c := &cobra.Command{
		Use:          "apply",
		Short:        "Apply a redaction rules file to a connected PostgreSQL cluster",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			plan, err := loadRedactPlan(rulesPath, saltHex)
			if err != nil {
				return err
			}
			tableSQLs := plan.SQL()
			if printSQL {
				for _, t := range tableSQLs {
					fmt.Fprintf(cmd.OutOrStdout(), "BEGIN;\n%s;\nCOMMIT;\n", t.Stmt)
				}
				return nil
			}
			// Flag-gated: --pg-connection only for a live run.
			if pgConn == "" && !dryRun {
				return missingFlagErr(cmd, "--pg-connection (or use --dry-run / --print-sql)")
			}
			if dryRun {
				return d.Result(output.NewResult(cmd.CommandPath()).WithBody(redactBody{
					DryRun:     true,
					Tables:     tableNames(tableSQLs),
					Statements: len(tableSQLs),
					SaltHex:    plan.SaltHex(),
				}))
			}

			ps, err := exec.LookPath("psql")
			if err != nil {
				return output.NewError("redact.psql_not_found",
					"redact apply: psql not on PATH (install postgresql-client or use --print-sql)").Wrap(err)
			}
			// One psql invocation per table so a per-table
			// failure surfaces as a distinct error and leaves
			// preceding tables redacted.
			for _, t := range tableSQLs {
				body := fmt.Sprintf("BEGIN;\n%s;\nCOMMIT;\n", t.Stmt)
				pcmd := exec.CommandContext(cmd.Context(), ps,
					"-v", "ON_ERROR_STOP=1",
					"-d", pgConn, "-X", "-q", "-1")
				pcmd.Stdin = strings.NewReader(body)
				pcmd.Stderr = cmd.ErrOrStderr()
				if err := pcmd.Run(); err != nil {
					return output.NewError("redact.apply_failed",
						fmt.Sprintf("redact apply: %s: %v", t.Table, err)).
						WithSuggestion(&output.Suggestion{
							Human:   fmt.Sprintf("inspect the failing table %q; rerun with --dry-run to see the SQL", t.Table),
							Command: fmt.Sprintf("pg_hardstorage redact apply --rules %s --dry-run", rulesPath),
						}).Wrap(err)
				}
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(redactBody{
				Tables:     tableNames(tableSQLs),
				Statements: len(tableSQLs),
				SaltHex:    plan.SaltHex(),
				Applied:    true,
			}))
		},
	}
	c.Flags().StringVar(&rulesPath, "rules", "",
		"path to a YAML rules file (required)")
	_ = c.MarkFlagRequired("rules")
	c.Flags().StringVar(&pgConn, "pg-connection", "",
		"libpq connection string for the target PostgreSQL cluster")
	c.Flags().BoolVar(&dryRun, "dry-run", false,
		"validate the plan without applying any SQL")
	c.Flags().BoolVar(&printSQL, "print-sql", false,
		"print the generated SQL on stdout (no PG connection required)")
	c.Flags().StringVar(&saltHex, "salt-hex", "",
		"override the random salt with a fixed hex string (use to reproduce identical hashes across runs)")
	return c
}

func newRedactPreviewCmd() *cobra.Command {
	var rulesPath string
	c := &cobra.Command{
		Use:          "preview",
		Short:        "Preview the redaction plan without touching the database",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			plan, err := loadRedactPlan(rulesPath, "")
			if err != nil {
				return err
			}
			tableSQLs := plan.SQL()
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(redactPreviewBody{
				Tables:     tableNames(tableSQLs),
				Statements: tableSQLs,
				SaltHex:    plan.SaltHex(),
			}))
		},
	}
	c.Flags().StringVar(&rulesPath, "rules", "",
		"path to a YAML rules file (required)")
	_ = c.MarkFlagRequired("rules")
	return c
}

func loadRedactPlan(rulesPath, saltHex string) (*redact.Plan, error) {
	body, err := os.ReadFile(rulesPath)
	if err != nil {
		return nil, output.NewError("redact.rules_read_failed",
			fmt.Sprintf("redact: read %s: %v", rulesPath, err)).Wrap(err)
	}
	rules, err := redact.ParseRules(body)
	if err != nil {
		return nil, output.NewError("redact.rules_invalid",
			fmt.Sprintf("redact: %v", err)).Wrap(err)
	}
	plan, err := redact.NewPlan(rules)
	if err != nil {
		return nil, output.NewError("redact.plan_failed", err.Error()).Wrap(err)
	}
	if saltHex != "" {
		decoded, err := decodeHex(saltHex)
		if err != nil {
			return nil, output.NewError("redact.bad_salt",
				fmt.Sprintf("redact: --salt-hex: %v", err)).Wrap(output.ErrUsage)
		}
		if err := plan.SetSalt(decoded); err != nil {
			return nil, output.NewError("redact.bad_salt", err.Error()).Wrap(output.ErrUsage)
		}
	}
	return plan, nil
}

func decodeHex(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("hex string must have even length; got %d", len(s))
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		hi, err := hexNybble(s[i])
		if err != nil {
			return nil, err
		}
		lo, err := hexNybble(s[i+1])
		if err != nil {
			return nil, err
		}
		out[i/2] = (hi << 4) | lo
	}
	return out, nil
}

func hexNybble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	}
	return 0, fmt.Errorf("invalid hex char %q", c)
}

func tableNames(sqls []redact.TableSQL) []string {
	out := make([]string, 0, len(sqls))
	for _, s := range sqls {
		out = append(out, s.Table)
	}
	return out
}

type redactBody struct {
	Tables     []string `json:"tables"`
	Statements int      `json:"statements"`
	SaltHex    string   `json:"salt_hex"`
	DryRun     bool     `json:"dry_run,omitempty"`
	Applied    bool     `json:"applied,omitempty"`
}

// WriteText renders the redaction outcome — statements run, salt, mode — as
// human-readable text to w.
func (b redactBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	verb := "applied"
	switch {
	case b.DryRun:
		verb = "dry-run (no SQL applied)"
	case !b.Applied:
		verb = "ready"
	}
	fmt.Fprintf(bw, "✓ redact %s\n", verb)
	fmt.Fprintf(bw, "  Tables:     %s\n", strings.Join(b.Tables, ", "))
	fmt.Fprintf(bw, "  Statements: %d\n", b.Statements)
	fmt.Fprintf(bw, "  Salt:       %s\n", b.SaltHex)
	fmt.Fprintf(bw, "  Note:       reuse --salt-hex %s to reproduce identical hashes on a future restore", b.SaltHex)
	_, err := io.WriteString(w, bw.String())
	return err
}

type redactPreviewBody struct {
	Tables     []string          `json:"tables"`
	Statements []redact.TableSQL `json:"statements"`
	SaltHex    string            `json:"salt_hex"`
}

// WriteText renders the per-table redaction SQL preview as human-readable
// text to w.
func (b redactPreviewBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "Redaction preview (salt=%s):\n", b.SaltHex)
	for _, t := range b.Statements {
		fmt.Fprintf(bw, "\n-- %s\n", t.Table)
		fmt.Fprintf(bw, "%s;\n", t.Stmt)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
