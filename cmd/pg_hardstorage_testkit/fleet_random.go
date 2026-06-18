// fleet_random.go — `fleet random` subcommand: seeded diversity-biased fleet generator from the catalog.
package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/catalog"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/random"
)

func newFleetRandomCmd() *cobra.Command {
	var (
		path           string
		count          int
		seed           int64
		preferPatroni  bool
		preferArm64    bool
		filesystemPool string
		archs          string
		force          bool
	)
	c := &cobra.Command{
		Use:   "random",
		Short: "Generate a randomised diversity-biased fleet from the catalog",
		Long: `Picks <count> cells from the catalog with bias toward distinct
OS families, distinct PG majors, and (optionally) at least one
Patroni cluster + arm64 cell.  Same --seed always produces the
same fleet.  Output goes to --file (default: ./fleet.yaml).

By default the picker uses every architecture the catalog
supports, which produces a mixed amd64+arm64 fleet.  That
requires QEMU/buildx for cross-arch image builds — most
soak runs pin to the host arch with --arch amd64 (or
arm64).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat, err := catalog.Default()
			if err != nil {
				return err
			}
			var fsPool []string
			if filesystemPool != "" {
				fsPool = strings.Split(filesystemPool, ",")
			}
			var archPool []string
			if archs != "" {
				archPool = strings.Split(archs, ",")
			}
			f, err := random.Pick(cat, random.Options{
				Count:          count,
				Seed:           seed,
				PreferPatroni:  preferPatroni,
				PreferArm64:    preferArm64,
				FilesystemPool: fsPool,
				ArchPool:       archPool,
			})
			if err != nil {
				return err
			}
			// random.Pick emits each (OS, PG, arch) leaf at most
			// once, so a result smaller than --count means the
			// catalog ran out of distinct cells.  Surface that
			// loudly: otherwise `--count 200` silently delivers
			// whatever the catalog tops out at (e.g. 32) and the
			// operator has no idea the number was clamped.
			if len(f.Entries) < count {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: requested --count %d, but the catalog has only %d distinct "+
						"cells for the selected arch(es) — fleet capped to %d\n",
					count, len(f.Entries), len(f.Entries))
			}
			if !force {
				if existsOnDisk(path) {
					return fmt.Errorf("fleet random: %s exists (pass --force to overwrite)", path)
				}
			}
			if err := saveFleetYAML(path, f); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"✓ wrote %d-cell fleet to %s (seed=%d)\n", len(f.Entries), path, seed)
			return nil
		},
	}
	c.Flags().StringVar(&path, "file", defaultFleetPath, "fleet YAML output path")
	c.Flags().IntVar(&count, "count", 5, "number of cells to pick")
	c.Flags().Int64Var(&seed, "seed", 0, "rng seed (same seed → same fleet)")
	c.Flags().BoolVar(&preferPatroni, "prefer-patroni", true,
		"force ≥1 Patroni cluster when count ≥5")
	c.Flags().BoolVar(&preferArm64, "prefer-arm64", false,
		"force ≥1 arm64 cell when count ≥4")
	c.Flags().StringVar(&filesystemPool, "filesystems", "",
		"comma-separated subset of {ext4,xfs,zfs,btrfs} (default: all)")
	c.Flags().StringVar(&archs, "arch", "",
		"comma-separated arch allow-list (e.g. amd64); default: all catalog arches (mixed amd64/arm64 fleet)")
	c.Flags().BoolVar(&force, "force", false, "overwrite existing file")
	return c
}
