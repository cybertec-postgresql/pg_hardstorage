// fleet.go — `fleet` subcommand: manage the test-cell fleet (OS x PG x arch matrix) YAML.
package main

import (
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/catalog"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
)

// defaultFleetPath is where fleet.yaml lives unless --file
// overrides.  testkit/ at the repo root keeps fleet/profile/
// fault YAML next to scenarios + load.
const defaultFleetPath = "test/fleet.yaml"

func newFleetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "fleet <list|add|edit|remove|validate|random|split>",
		Short: "Manage the test-cell fleet (OS × PG × arch matrix)",
	}
	c.AddCommand(
		newFleetListCmd(),
		newFleetAddCmd(),
		newFleetEditCmd(),
		newFleetRemoveCmd(),
		newFleetValidateCmd(),
		newFleetRandomCmd(),
		newFleetSplitCmd(),
	)
	return c
}

func newFleetListCmd() *cobra.Command {
	var path string
	c := &cobra.Command{
		Use:   "list",
		Short: "Show every fleet entry as a table",
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := config.LoadFleet(path)
			if err != nil {
				return err
			}
			f.SortByName()
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tOS\tPG\tARCH\tCOUNT\tROLE\tNODES\tFS\tSTORAGE\tSINK")
			for _, e := range f.Entries {
				nodes := ""
				if e.Nodes > 0 {
					nodes = strconv.Itoa(e.Nodes)
				}
				sink := e.Sink
				if sink == "" {
					sink = "file"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%dGB\t%s\n",
					e.Name, e.OS, e.PG, e.EffectiveArch(), e.Count,
					e.EffectiveRole(), nodes,
					e.EffectiveFilesystem(), e.EffectiveStorageGB(), sink)
			}
			if len(f.Entries) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(),
					"(no entries — run `fleet add` to create one)\n")
				return nil
			}
			return tw.Flush()
		},
	}
	c.Flags().StringVar(&path, "file", defaultFleetPath, "fleet YAML path")
	return c
}

func newFleetAddCmd() *cobra.Command {
	var (
		path  string
		entry config.FleetEntry
	)
	c := &cobra.Command{
		Use:   "add",
		Short: "Add a new fleet entry (interactive when stdin is a TTY)",
		Long: `Add a fleet entry.  Every required field can be supplied via flags
for non-interactive / CI use; missing fields trigger an interactive
wizard when stdin is a terminal.  Validation against the catalog
runs before the entry lands on disk — typos surface immediately.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat, err := catalog.Default()
			if err != nil {
				return err
			}
			f, err := config.LoadFleet(path)
			if err != nil {
				return err
			}

			// Interactive fill-in if any required field is empty
			// AND stdin is a terminal.
			if needsWizard(entry) {
				if !stdinIsTTY() {
					return fmt.Errorf("fleet add: missing required flags and stdin is not a TTY (use --name --os --pg --count, etc.)")
				}
				if err := promptFleetEntry(cmd, cat, &entry); err != nil {
					return err
				}
			}

			// Always validate before saving.
			if err := validateEntryAgainstCatalog(entry, cat); err != nil {
				return err
			}
			if err := f.AddEntry(entry); err != nil {
				return err
			}
			if err := config.SaveFleet(path, f); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"✓ added entry %q to %s (%s + PG %s × %s, count=%d)\n",
				entry.Name, path, entry.OS, entry.PG, entry.EffectiveArch(), entry.Count)
			return nil
		},
	}
	c.Flags().StringVar(&path, "file", defaultFleetPath, "fleet YAML path")
	c.Flags().StringVar(&entry.Name, "name", "", "entry name (unique within the fleet)")
	c.Flags().StringVar(&entry.OS, "os", "", "OS id (e.g. ubuntu:24.04 — must be in the catalog)")
	c.Flags().StringVar(&entry.PG, "pg", "", "PostgreSQL version (e.g. 17, 18-dev)")
	c.Flags().StringVar(&entry.Arch, "arch", "", "amd64 (default) | arm64")
	c.Flags().IntVar(&entry.Count, "count", 0, "number of cells (containers) of this type")
	c.Flags().StringVar(&entry.Role, "role", "", "standalone (default) | primary | replica | patroni-cluster")
	c.Flags().IntVar(&entry.Nodes, "nodes", 0, "PG nodes per Patroni cluster (only for role=patroni-cluster)")
	c.Flags().StringVar(&entry.Filesystem, "filesystem", "", "ext4 (default) | xfs | zfs | btrfs")
	c.Flags().IntVar(&entry.StorageGB, "storage-gb", 0, "loopback disk size in GB (default 10)")
	return c
}

func newFleetEditCmd() *cobra.Command {
	var path string
	c := &cobra.Command{
		Use:   "edit <name>",
		Short: "Edit an existing fleet entry interactively",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cat, err := catalog.Default()
			if err != nil {
				return err
			}
			f, err := config.LoadFleet(path)
			if err != nil {
				return err
			}
			cur := f.FindEntry(name)
			if cur == nil {
				return fmt.Errorf("fleet edit: no entry named %q (run `fleet list` to see what exists)", name)
			}
			if !stdinIsTTY() {
				return fmt.Errorf("fleet edit: stdin is not a TTY; use `remove` + `add --name=…` for scripted edits")
			}
			updated := *cur
			if err := promptFleetEntry(cmd, cat, &updated); err != nil {
				return err
			}
			if err := validateEntryAgainstCatalog(updated, cat); err != nil {
				return err
			}
			if err := f.ReplaceEntry(updated); err != nil {
				return err
			}
			if err := config.SaveFleet(path, f); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ updated entry %q in %s\n", name, path)
			return nil
		},
	}
	c.Flags().StringVar(&path, "file", defaultFleetPath, "fleet YAML path")
	return c
}

func newFleetRemoveCmd() *cobra.Command {
	var path string
	c := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a fleet entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := config.LoadFleet(path)
			if err != nil {
				return err
			}
			if err := f.RemoveEntry(args[0]); err != nil {
				return fmt.Errorf("fleet remove: %s: %w", args[0], err)
			}
			if err := config.SaveFleet(path, f); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ removed entry %q from %s\n", args[0], path)
			return nil
		},
	}
	c.Flags().StringVar(&path, "file", defaultFleetPath, "fleet YAML path")
	return c
}

func newFleetValidateCmd() *cobra.Command {
	var path string
	c := &cobra.Command{
		Use:   "validate",
		Short: "Type-check the fleet YAML against the catalog",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat, err := catalog.Default()
			if err != nil {
				return err
			}
			f, err := config.LoadFleet(path)
			if err != nil {
				return err
			}
			if err := f.Validate(cat); err != nil {
				return err
			}
			total := 0
			for _, e := range f.Entries {
				total += e.EffectiveContainerCount()
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"✓ %s is valid (%d entries, expanding to %d containers)\n",
				path, len(f.Entries), total)
			return nil
		},
	}
	c.Flags().StringVar(&path, "file", defaultFleetPath, "fleet YAML path")
	return c
}

// --- shared helpers ---------------------------------------------------

func needsWizard(e config.FleetEntry) bool {
	return e.Name == "" || e.OS == "" || e.PG == "" || e.Count == 0
}

func validateEntryAgainstCatalog(e config.FleetEntry, c *catalog.Catalog) error {
	// Reuse the Fleet-level validator on a single-entry fleet.
	tmp := &config.Fleet{Schema: config.FleetSchema, Version: 1, Entries: []config.FleetEntry{e}}
	return tmp.Validate(c)
}

// promptFleetEntry walks the user through every field, defaulting
// to the values already on `e` so `edit` round-trips cleanly.
func promptFleetEntry(cmd *cobra.Command, cat *catalog.Catalog, e *config.FleetEntry) error {
	w := newWizard(os.Stdin, cmd.OutOrStdout())

	if e.Name == "" {
		name, err := w.text("Name", "", false)
		if err != nil {
			return err
		}
		e.Name = name
	}

	if e.OS == "" || !catalogContainsOS(cat, e.OS) {
		os_, err := w.choices("OS:", cat.OSIDs(), e.OS)
		if err != nil {
			return err
		}
		e.OS = os_
	}

	o, _ := cat.FindOS(e.OS)
	if e.PG == "" || !o.SupportsPG(e.PG) {
		pg, err := w.choices(
			fmt.Sprintf("PG version (%s supports):", e.OS),
			o.PGVersions, e.PG)
		if err != nil {
			return err
		}
		e.PG = pg
	}

	if e.Arch == "" || !o.SupportsArch(e.Arch) {
		arch, err := w.choices("Architecture:", o.Arches, "amd64")
		if err != nil {
			return err
		}
		e.Arch = arch
	}

	if e.Count == 0 {
		count, err := w.integer("Count (number of containers)", 1)
		if err != nil {
			return err
		}
		e.Count = count
	}

	if e.Role == "" {
		role, err := w.choices("Role:", cat.Roles, "standalone")
		if err != nil {
			return err
		}
		e.Role = role
	}

	if e.Role == "patroni-cluster" && e.Nodes == 0 {
		nodes, err := w.integer("PG nodes per Patroni cluster", 3)
		if err != nil {
			return err
		}
		e.Nodes = nodes
	}

	if e.Filesystem == "" {
		fs, err := w.choices("Filesystem:", cat.Filesystems, "ext4")
		if err != nil {
			return err
		}
		e.Filesystem = fs
	}

	if e.StorageGB == 0 {
		gb, err := w.integer("Loopback disk size (GB)", 10)
		if err != nil {
			return err
		}
		e.StorageGB = gb
	}

	return nil
}

func catalogContainsOS(c *catalog.Catalog, id string) bool {
	for _, o := range c.OSes {
		if o.ID == id {
			return true
		}
	}
	return false
}
