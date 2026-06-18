// fleet_split.go — `fleet split` subcommand: partitions a fleet YAML into bounded-size batch files.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
)

// newFleetSplitCmd partitions a fleet YAML into fixed-size batch
// files.  run_testing.sh --max-containers uses it to soak a large
// OS×PG matrix in bounded-concurrency chunks: the full fleet is
// generated once, split here, and each batch is then brought up /
// soaked / torn down in turn so only one batch's containers are
// ever live at once.
//
// The split preserves entry order, so batch N always holds the
// same cells for a given input fleet — reproducibility is the
// contract, exactly as for `fleet random`.
func newFleetSplitCmd() *cobra.Command {
	var (
		path   string
		size   int
		outDir string
	)
	c := &cobra.Command{
		Use:   "split",
		Short: "Partition a fleet into fixed-size batch files",
		Long: `Reads a fleet YAML and writes it back out as batch files of at
most --size entries each (batch-001.yaml, batch-002.yaml, … in
--out-dir).  Used by run_testing.sh --max-containers to run a
large matrix in bounded-concurrency chunks.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if size < 1 {
				return fmt.Errorf("fleet split: --size must be ≥1 (got %d)", size)
			}
			f, err := config.LoadFleet(path)
			if err != nil {
				return err
			}
			if len(f.Entries) == 0 {
				return fmt.Errorf("fleet split: %s has no entries", path)
			}
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return fmt.Errorf("fleet split: mkdir %s: %w", outDir, err)
			}
			batches := 0
			for start := 0; start < len(f.Entries); start += size {
				end := start + size
				if end > len(f.Entries) {
					end = len(f.Entries)
				}
				batches++
				batch := &config.Fleet{
					Schema:  f.Schema,
					Version: f.Version,
					Entries: f.Entries[start:end],
				}
				bp := filepath.Join(outDir, fmt.Sprintf("batch-%03d.yaml", batches))
				if err := config.SaveFleet(bp, batch); err != nil {
					return err
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"✓ split %d-entry fleet into %d batch file(s) of ≤%d entries in %s\n",
				len(f.Entries), batches, size, outDir)
			return nil
		},
	}
	c.Flags().StringVar(&path, "file", defaultFleetPath, "input fleet YAML")
	c.Flags().IntVar(&size, "size", 8, "max entries per batch file")
	c.Flags().StringVar(&outDir, "out-dir", "fleet-batches", "directory to write batch files into")
	return c
}
