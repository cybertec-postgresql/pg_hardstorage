// fault.go — `fault` subcommand: list/add/edit/remove/validate entries in the fault-injection vocabulary.
package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
)

const defaultFaultsPath = "test/faults.yaml"

func newFaultCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "fault <list|add|edit|remove|validate>",
		Short: "Manage the fault-injection vocabulary",
	}
	c.AddCommand(
		newFaultListCmd(),
		newFaultAddCmd(),
		newFaultEditCmd(),
		newFaultRemoveCmd(),
		newFaultValidateCmd(),
	)
	return c
}

func newFaultListCmd() *cobra.Command {
	var path string
	c := &cobra.Command{
		Use: "list", Short: "Show every fault in the vocabulary",
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := config.LoadFaults(path)
			if err != nil {
				return err
			}
			f.SortByName()
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tWEIGHT\tACTION")
			for _, e := range f.Faults {
				fmt.Fprintf(tw, "%s\t%d\t%s\n", e.Name, e.Weight, e.Action)
			}
			if len(f.Faults) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no faults — run `fault add` to create one)")
				return nil
			}
			return tw.Flush()
		},
	}
	c.Flags().StringVar(&path, "file", defaultFaultsPath, "faults YAML path")
	return c
}

func newFaultAddCmd() *cobra.Command {
	var (
		path string
		e    config.Fault
	)
	c := &cobra.Command{
		Use: "add", Short: "Add a fault to the vocabulary",
		Long: `Add a fault entry.  The action string must start with one of
the recognised primitives (run with --help to see the list).
The actual injection logic lives in the inject package which
ships separately; this command only manages the catalogue.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := config.LoadFaults(path)
			if err != nil {
				return err
			}
			if e.Name == "" || e.Action == "" {
				if !stdinIsTTY() {
					return fmt.Errorf("fault add: missing required flags (use --name --action)")
				}
				if err := promptFault(cmd, &e); err != nil {
					return err
				}
			}
			if err := f.AddFault(e); err != nil {
				return err
			}
			if err := f.Validate(); err != nil {
				return err
			}
			if err := config.SaveFaults(path, f); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"✓ added fault %q (weight=%d, action=%s)\n", e.Name, e.Weight, e.Action)
			return nil
		},
	}
	c.Flags().StringVar(&path, "file", defaultFaultsPath, "faults YAML path")
	c.Flags().StringVar(&e.Name, "name", "", "fault name")
	c.Flags().IntVar(&e.Weight, "weight", 1, "drive-loop selection weight (0 = never)")
	c.Flags().StringVar(&e.Action, "action", "",
		"action string — must start with one of: "+strings.Join(config.KnownActionPrefixes(), ", "))
	return c
}

func newFaultEditCmd() *cobra.Command {
	var path string
	c := &cobra.Command{
		Use: "edit <name>", Short: "Edit a fault interactively",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := config.LoadFaults(path)
			if err != nil {
				return err
			}
			cur := f.FindFault(args[0])
			if cur == nil {
				return fmt.Errorf("fault edit: no fault %q", args[0])
			}
			if !stdinIsTTY() {
				return fmt.Errorf("fault edit: stdin is not a TTY")
			}
			updated := *cur
			if err := promptFault(cmd, &updated); err != nil {
				return err
			}
			if err := f.ReplaceFault(updated); err != nil {
				return err
			}
			if err := f.Validate(); err != nil {
				return err
			}
			return config.SaveFaults(path, f)
		},
	}
	c.Flags().StringVar(&path, "file", defaultFaultsPath, "faults YAML path")
	return c
}

func newFaultRemoveCmd() *cobra.Command {
	var path string
	c := &cobra.Command{
		Use: "remove <name>", Short: "Remove a fault",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := config.LoadFaults(path)
			if err != nil {
				return err
			}
			if err := f.RemoveFault(args[0]); err != nil {
				return fmt.Errorf("fault remove: %s: %w", args[0], err)
			}
			if err := config.SaveFaults(path, f); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ removed fault %q\n", args[0])
			return nil
		},
	}
	c.Flags().StringVar(&path, "file", defaultFaultsPath, "faults YAML path")
	return c
}

func newFaultValidateCmd() *cobra.Command {
	var path string
	c := &cobra.Command{
		Use: "validate", Short: "Type-check the faults YAML",
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := config.LoadFaults(path)
			if err != nil {
				return err
			}
			if err := f.Validate(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s is valid (%d faults)\n", path, len(f.Faults))
			return nil
		},
	}
	c.Flags().StringVar(&path, "file", defaultFaultsPath, "faults YAML path")
	return c
}

func promptFault(cmd *cobra.Command, e *config.Fault) error {
	w := newWizard(os.Stdin, cmd.OutOrStdout())
	if e.Name == "" {
		n, err := w.text("Name", "", false)
		if err != nil {
			return err
		}
		e.Name = n
	}
	if e.Weight == 0 {
		wt, err := w.integer("Weight (drive-loop selection)", 5)
		if err != nil {
			return err
		}
		e.Weight = wt
	}
	if e.Action == "" {
		// Pick the prefix first, then ask for the args portion.
		prefix, err := w.choices("Action primitive:", config.KnownActionPrefixes(), "")
		if err != nil {
			return err
		}
		args, err := w.text(
			fmt.Sprintf("Args for %s (e.g. \"target=repo, fill=98%%\")", prefix),
			"", true)
		if err != nil {
			return err
		}
		if args == "" {
			e.Action = prefix + "()"
		} else {
			e.Action = fmt.Sprintf("%s(%s)", prefix, args)
		}
	}
	return nil
}
