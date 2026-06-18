// profile.go — `profile` subcommand: list/add/edit/remove/validate workload profiles (size/churn/schema).
package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
)

const defaultProfilesPath = "test/profiles.yaml"

func newProfileCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "profile <list|add|edit|remove|validate>",
		Short: "Manage workload profiles (size / churn / schema)",
	}
	c.AddCommand(
		newProfileListCmd(),
		newProfileAddCmd(),
		newProfileEditCmd(),
		newProfileRemoveCmd(),
		newProfileValidateCmd(),
	)
	return c
}

func newProfileListCmd() *cobra.Command {
	var path string
	c := &cobra.Command{
		Use: "list", Short: "Show every profile",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := config.LoadProfiles(path)
			if err != nil {
				return err
			}
			p.SortByName()
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSIZE_GB\tCHURN_MB/MIN\tTABLES\tSCHEMA\tBACKUP_EVERY\tDDL/MIN\tNO_HOST")
			for _, e := range p.Profiles {
				fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\t%s\t%d\t%t\n",
					e.Name, e.TargetSizeGB, e.ChurnMBPerMin,
					e.TableCount, e.Schema, e.BackupEvery, e.DDLPerMin, e.NoHostAccess)
			}
			if len(p.Profiles) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no profiles — run `profile add` to create one)")
				return nil
			}
			return tw.Flush()
		},
	}
	c.Flags().StringVar(&path, "file", defaultProfilesPath, "profiles YAML path")
	return c
}

func newProfileAddCmd() *cobra.Command {
	var (
		path string
		e    config.Profile
	)
	c := &cobra.Command{
		Use: "add", Short: "Add a workload profile",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := config.LoadProfiles(path)
			if err != nil {
				return err
			}
			if e.Name == "" || e.TargetSizeGB == 0 {
				if !stdinIsTTY() {
					return fmt.Errorf("profile add: missing required flags (use --name --target-size-gb)")
				}
				if err := promptProfile(cmd, &e); err != nil {
					return err
				}
			}
			if err := p.AddProfile(e); err != nil {
				return err
			}
			if err := p.Validate(); err != nil {
				return err
			}
			if err := config.SaveProfiles(path, p); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ added profile %q (size=%dGB)\n", e.Name, e.TargetSizeGB)
			return nil
		},
	}
	c.Flags().StringVar(&path, "file", defaultProfilesPath, "profiles YAML path")
	c.Flags().StringVar(&e.Name, "name", "", "profile name")
	c.Flags().IntVar(&e.TargetSizeGB, "target-size-gb", 0, "approximate dataset size (label / hint)")
	c.Flags().IntVar(&e.SeedTargetGB, "seed-target-gb", 0,
		"one-shot bulk seed via pgbench before iterations start (0 = skip)")
	c.Flags().IntVar(&e.ChurnMBPerMin, "churn-mb-per-min", 0, "continuous-churn rate (0 = none)")
	c.Flags().IntVar(&e.TableCount, "table-count", 0, "hint for the load engine")
	c.Flags().StringVar(&e.Schema, "schema", "", "tpcc-lite | fact-tables | bulk-copy")
	c.Flags().StringVar(&e.BackupEvery, "backup-every", "", "duration string (e.g. 5m, 1h)")
	c.Flags().IntVar(&e.DDLPerMin, "ddl-per-min", 0, "schema-churn rate")
	c.Flags().BoolVar(&e.NoHostAccess, "no-host-access", false, "simulate a managed-PG endpoint")
	return c
}

func newProfileEditCmd() *cobra.Command {
	var path string
	c := &cobra.Command{
		Use: "edit <name>", Short: "Edit a profile interactively",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := config.LoadProfiles(path)
			if err != nil {
				return err
			}
			cur := p.FindProfile(args[0])
			if cur == nil {
				return fmt.Errorf("profile edit: no profile %q", args[0])
			}
			if !stdinIsTTY() {
				return fmt.Errorf("profile edit: stdin is not a TTY")
			}
			updated := *cur
			if err := promptProfile(cmd, &updated); err != nil {
				return err
			}
			if err := p.ReplaceProfile(updated); err != nil {
				return err
			}
			if err := p.Validate(); err != nil {
				return err
			}
			return config.SaveProfiles(path, p)
		},
	}
	c.Flags().StringVar(&path, "file", defaultProfilesPath, "profiles YAML path")
	return c
}

func newProfileRemoveCmd() *cobra.Command {
	var path string
	c := &cobra.Command{
		Use: "remove <name>", Short: "Remove a profile",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := config.LoadProfiles(path)
			if err != nil {
				return err
			}
			if err := p.RemoveProfile(args[0]); err != nil {
				return fmt.Errorf("profile remove: %s: %w", args[0], err)
			}
			if err := config.SaveProfiles(path, p); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ removed profile %q\n", args[0])
			return nil
		},
	}
	c.Flags().StringVar(&path, "file", defaultProfilesPath, "profiles YAML path")
	return c
}

func newProfileValidateCmd() *cobra.Command {
	var path string
	c := &cobra.Command{
		Use: "validate", Short: "Type-check the profiles YAML",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := config.LoadProfiles(path)
			if err != nil {
				return err
			}
			if err := p.Validate(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s is valid (%d profiles)\n", path, len(p.Profiles))
			return nil
		},
	}
	c.Flags().StringVar(&path, "file", defaultProfilesPath, "profiles YAML path")
	return c
}

// promptProfile walks the user through profile fields,
// defaulting to current values for `edit`.
func promptProfile(cmd *cobra.Command, e *config.Profile) error {
	w := newWizard(os.Stdin, cmd.OutOrStdout())
	if e.Name == "" {
		n, err := w.text("Name", "", false)
		if err != nil {
			return err
		}
		e.Name = n
	}
	if e.TargetSizeGB == 0 {
		gb, err := w.integer("Target dataset size (GB)", 10)
		if err != nil {
			return err
		}
		e.TargetSizeGB = gb
	}
	if e.ChurnMBPerMin == 0 {
		c, err := w.integer("Churn (MB/min, 0 = none)", 0)
		if err != nil {
			return err
		}
		e.ChurnMBPerMin = c
	}
	if e.Schema == "" {
		s, err := w.choices("Schema:",
			[]string{"tpcc-lite", "fact-tables", "bulk-copy", "custom"}, "tpcc-lite")
		if err != nil {
			return err
		}
		e.Schema = s
	}
	if e.BackupEvery == "" {
		be, err := w.text("Backup cadence (duration, e.g. 5m)", "5m", true)
		if err != nil {
			return err
		}
		e.BackupEvery = be
	}
	return nil
}
