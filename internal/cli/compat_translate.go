// compat_translate.go — 'compat translate' CLI verb: pgBackRest/Barman/WAL-G config → pg_hardstorage.yaml.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	// pgBackRest path lives in compat/pgbackrest/translate/
	pgbackresttranslate "github.com/cybertec-postgresql/pg_hardstorage/compat/pgbackrest/translate"
	// Barman path lives in compat/barman/translate/
	barmantranslate "github.com/cybertec-postgresql/pg_hardstorage/compat/barman/translate"
	// WAL-G path lives in compat/walg/translate/
	walgtranslate "github.com/cybertec-postgresql/pg_hardstorage/compat/walg/translate"
)

// newCompatTranslateCmd implements `pg_hardstorage compat translate`.
//
// Reads a legacy config file, renders the equivalent
// pg_hardstorage.yaml, and surfaces a stderr summary of the
// settings that didn't make it across.
//
// The file is written to --output (default: stdout).  We
// never overwrite a non-empty target without --force.
func newCompatTranslateCmd() *cobra.Command {
	var (
		from   string
		output string
		force  bool
	)
	c := &cobra.Command{
		Use:   "translate --from <tool> <config-path>",
		Short: "Convert a legacy config to pg_hardstorage.yaml",
		Long: `translate reads a legacy backup-tool config file (today:
pgBackRest's pgbackrest.conf, Barman's barman.conf, or a
WAL-G env file) and writes the equivalent pg_hardstorage.yaml.
Every setting that doesn't have a direct semantic equivalent
is emitted as a YAML comment with a stderr summary so the
operator can review.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCompatTranslate(from, args[0], output, force)
		},
	}
	c.Flags().StringVar(&from, "from", "",
		"source tool: pgbackrest|barman|walg (required)")
	c.Flags().StringVar(&output, "output", "",
		"output file path (default: stdout)")
	c.Flags().BoolVar(&force, "force", false,
		"overwrite an existing output file")
	_ = c.MarkFlagRequired("from")
	return c
}

func runCompatTranslate(from, input, output string, force bool) error {
	switch from {
	case "pgbackrest":
		return translatePgbackrest(input, output, force)
	case "barman":
		return translateBarman(input, output, force)
	case "walg", "wal-g":
		return translateWalg(input, output, force)
	default:
		return fmt.Errorf(
			"compat translate: --from %q not supported (supported: pgbackrest, barman, walg)",
			from)
	}
}

func translatePgbackrest(input, output string, force bool) error {
	in, err := os.Open(input)
	if err != nil {
		return fmt.Errorf("compat translate: open %s: %w", input, err)
	}
	defer in.Close()

	cfg, err := pgbackresttranslate.Parse(in)
	if err != nil {
		return fmt.Errorf("compat translate: parse %s: %w", input, err)
	}

	res, err := pgbackresttranslate.Translate(cfg)
	if err != nil {
		return fmt.Errorf("compat translate: %w", err)
	}

	// Write YAML.
	if output == "" {
		fmt.Print(res.YAML)
	} else {
		if !force {
			if _, err := os.Stat(output); err == nil {
				return fmt.Errorf(
					"compat translate: %s exists (pass --force to overwrite)",
					output)
			}
		}
		// 0o600: the YAML may carry connection strings with
		// credentials translated from inlined pgbackrest.conf /
		// barman.conf entries, plus repo URLs that can include
		// S3 access keys.  Match the keystore convention.
		if err := os.WriteFile(output, []byte(res.YAML), 0o600); err != nil {
			return fmt.Errorf("compat translate: write %s: %w", output, err)
		}
		fmt.Fprintf(os.Stderr, "compat translate: wrote %s\n", output)
	}

	// Stderr summary.
	if len(res.Warnings) > 0 {
		fmt.Fprintln(os.Stderr, "compat translate: semantic-equivalence notes:")
		for _, w := range res.Warnings {
			fmt.Fprintln(os.Stderr, "  -", w)
		}
	}
	if len(res.Unmapped) > 0 {
		fmt.Fprintln(os.Stderr, "compat translate: unmapped settings (review manually):")
		for _, u := range res.Unmapped {
			fmt.Fprintln(os.Stderr, "  -", u)
		}
	}
	return nil
}

// translateBarman is the --from=barman path.  Reads a Barman INI
// (single multi-section file or one-server-per-file) and renders
// the equivalent pg_hardstorage.yaml via compat/barman/translate.
func translateBarman(input, output string, force bool) error {
	in, err := os.Open(input)
	if err != nil {
		return fmt.Errorf("compat translate: open %s: %w", input, err)
	}
	defer in.Close()

	res, err := barmantranslate.Translate(in)
	if err != nil {
		return fmt.Errorf("compat translate: parse %s: %w", input, err)
	}

	if output == "" {
		fmt.Print(res.YAML)
	} else {
		if !force {
			if _, err := os.Stat(output); err == nil {
				return fmt.Errorf(
					"compat translate: %s exists (pass --force to overwrite)",
					output)
			}
		}
		// 0o600: the YAML may carry connection strings with
		// credentials translated from inlined pgbackrest.conf /
		// barman.conf entries, plus repo URLs that can include
		// S3 access keys.  Match the keystore convention.
		if err := os.WriteFile(output, []byte(res.YAML), 0o600); err != nil {
			return fmt.Errorf("compat translate: write %s: %w", output, err)
		}
		fmt.Fprintf(os.Stderr, "compat translate: wrote %s\n", output)
	}

	if len(res.Unmapped) > 0 {
		fmt.Fprintln(os.Stderr, "compat translate: unmapped Barman settings (review manually):")
		for _, u := range res.Unmapped {
			if u.Section == "" {
				fmt.Fprintf(os.Stderr, "  - [barman] %s = %s  (%s)\n", u.Key, u.Value, u.Reason)
			} else {
				fmt.Fprintf(os.Stderr, "  - [%s] %s = %s  (%s)\n", u.Section, u.Key, u.Value, u.Reason)
			}
		}
	}
	return nil
}

// translateWalg is the --from=walg path.  Reads a WAL-G env-file
// (one KEY=VALUE per line, optional `export ` prefix) and renders
// the equivalent pg_hardstorage.yaml via compat/walg/translate.
func translateWalg(input, output string, force bool) error {
	in, err := os.Open(input)
	if err != nil {
		return fmt.Errorf("compat translate: open %s: %w", input, err)
	}
	defer in.Close()

	env, err := walgtranslate.Parse(in)
	if err != nil {
		return fmt.Errorf("compat translate: parse %s: %w", input, err)
	}

	res, err := walgtranslate.Translate(env)
	if err != nil {
		return fmt.Errorf("compat translate: %w", err)
	}

	if output == "" {
		fmt.Print(res.YAML)
	} else {
		if !force {
			if _, err := os.Stat(output); err == nil {
				return fmt.Errorf(
					"compat translate: %s exists (pass --force to overwrite)",
					output)
			}
		}
		// 0o600: WAL-G env files frequently include
		// AWS_SECRET_ACCESS_KEY / WALG_LIBSODIUM_KEY / GPG keys
		// inline; the rendered YAML may inherit credentials we
		// extracted from those.  Match the keystore convention.
		if err := os.WriteFile(output, []byte(res.YAML), 0o600); err != nil {
			return fmt.Errorf("compat translate: write %s: %w", output, err)
		}
		fmt.Fprintf(os.Stderr, "compat translate: wrote %s\n", output)
	}

	if len(res.Warnings) > 0 {
		fmt.Fprintln(os.Stderr, "compat translate: semantic-equivalence notes:")
		for _, w := range res.Warnings {
			fmt.Fprintln(os.Stderr, "  -", w)
		}
	}
	if len(res.Unmapped) > 0 {
		fmt.Fprintln(os.Stderr, "compat translate: unmapped settings (review manually):")
		for _, u := range res.Unmapped {
			fmt.Fprintln(os.Stderr, "  -", u)
		}
	}
	return nil
}
