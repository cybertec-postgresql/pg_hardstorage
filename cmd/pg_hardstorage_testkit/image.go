// image.go — `image` subcommand: list/build/pull/push testbed images and pre-fetch sink emulator images.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/catalog"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/imagetag"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/sink"
)

const (
	defaultImageRepo     = "ghcr.io/cybertec-postgresql/pg-hardstorage-testbed"
	defaultDockerfileDir = "dockerfiles/testbed"
)

func newImageCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "image <list|catalog|build|pull|push|pull-sinks>",
		Short: "Manage testbed + sink container images",
	}
	c.AddCommand(
		newImageListCmd(),
		newImageCatalogCmd(),
		newImageBuildCmd(),
		newImagePullCmd(),
		newImagePushCmd(),
		newImagePullSinksCmd(),
	)
	return c
}

// newImagePullSinksCmd implements `pg_hardstorage_testkit image
// pull-sinks` — pre-fetches every emulator image the testkit's
// sink package knows about (MinIO for S3, Azurite for Azure
// Blob, fake-gcs-server for GCS, atmoz/sftp for SFTP).
//
// Why this exists
// ---------------
// Air-gap operation needs every image that any scenario / soak
// might bring up to be already-local.  Without a single
// command that knows the canonical set, operators are stuck
// reading docs to find the right list of `docker pull` lines.
// This subcommand IS that list, code-defined and pinned.
//
// The set tracks internal/testkit/sink.SinkImages exactly —
// adding a new sink kind means adding it to that map and
// nothing else.
func newImagePullSinksCmd() *cobra.Command {
	var only string
	c := &cobra.Command{
		Use:   "pull-sinks",
		Short: "Pre-pull every sink emulator image (for air-gap operation)",
		Long: `Pulls the canonical set of sink emulator images so subsequent
scenario / soak runs work entirely offline.  Image tags are
pinned in internal/testkit/sink.SinkImages; bumps land as
their own commits.

Use --only=s3-minio,azurite to limit the pull to a subset.

Once this command has succeeded, every sink-enabled scenario
runs without network — pass --airgap to validate / scenario
run to enforce that promise (refuses to dial out at runtime).`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runImagePullSinks(cmd, only)
		},
	}
	c.Flags().StringVar(&only, "only", "",
		"comma-separated list of sink kinds to pull (default: all)")
	return c
}

func runImagePullSinks(cmd *cobra.Command, only string) error {
	wanted := sink.SinkImages
	if only != "" {
		wanted = map[string]string{}
		for _, k := range strings.Split(only, ",") {
			k = strings.TrimSpace(k)
			img, ok := sink.SinkImages[k]
			if !ok {
				return fmt.Errorf("image pull-sinks: unknown sink kind %q (known: %v)",
					k, sink.KnownKinds())
			}
			wanted[k] = img
		}
	}

	// Sort so output is stable across runs — predictable
	// for CI log diffs and operator muscle memory.
	kinds := make([]string, 0, len(wanted))
	for k := range wanted {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	out := cmd.OutOrStdout()
	for _, k := range kinds {
		img := wanted[k]
		fmt.Fprintf(out, "→ pulling %s (%s)\n", k, img)
		c := exec.CommandContext(cmd.Context(), "docker", "pull", img)
		c.Stdout = out
		c.Stderr = cmd.ErrOrStderr()
		if err := c.Run(); err != nil {
			return fmt.Errorf("image pull-sinks: docker pull %s: %w", img, err)
		}
		fmt.Fprintf(out, "✓ %s ready\n", k)
	}
	fmt.Fprintf(out, "\n%d sink image(s) ready for offline use.\n", len(kinds))
	return nil
}

func newImageListCmd() *cobra.Command {
	var repo string
	c := &cobra.Command{
		Use:   "list",
		Short: "Show every (os, pg, arch) cell the catalog supports + its image tag",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat, err := catalog.Default()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "OS\tPG\tARCH\tFAMILY\tPACKAGES\tIMAGE_TAG")
			for _, o := range cat.OSes {
				pkg := cat.EffectivePackages(&o)
				for _, pg := range o.PGVersions {
					for _, arch := range o.Arches {
						tag := imageTag(repo, o.ID, pg, arch)
						fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
							o.ID, pg, arch, o.Family, pkg, tag)
					}
				}
			}
			return tw.Flush()
		},
	}
	c.Flags().StringVar(&repo, "repo", defaultImageRepo, "image registry repo prefix")
	return c
}

func newImageCatalogCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "catalog",
		Short: "Print the source-of-truth catalog (oses.yaml)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat, err := catalog.Default()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"catalog v%d (schema=%s, %d OSes, %d filesystems, %d roles)\n",
				cat.Version, cat.Schema, len(cat.OSes), len(cat.Filesystems), len(cat.Roles))
			fmt.Fprintln(cmd.OutOrStdout(), "")
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "OS\tFAMILY\tPG_VERSIONS\tARCHES\tPACKAGES")
			for _, o := range cat.OSes {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					o.ID, o.Family,
					strings.Join(o.PGVersions, ","),
					strings.Join(o.Arches, ","),
					cat.EffectivePackages(&o))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "")
			fmt.Fprintf(cmd.OutOrStdout(), "filesystems: %s\n", strings.Join(cat.Filesystems, ", "))
			fmt.Fprintf(cmd.OutOrStdout(), "roles:       %s\n", strings.Join(cat.Roles, ", "))
			return nil
		},
	}
}

func newImageBuildCmd() *cobra.Command {
	var (
		repo        string
		dfDir       string
		filterOS    string
		filterPG    string
		filterArch  string
		fromFleet   string
		dryRun      bool
		parallelism int
	)
	c := &cobra.Command{
		Use:   "build",
		Short: "Build testbed images for every catalog cell (or filter)",
		Long: `Walks the catalog and, for each (os, pg, arch) cell, runs
` + "`docker build`" + ` against the appropriate family Dockerfile.

By default builds every cell.  Filter with --only-os / --only-pg /
--only-arch (each is a substring match).  --from-fleet
<path> picks just the (os, pg, arch) tuples a fleet.yaml
references — typical for soak runs where the operator only
needs the few images the picked fleet uses.  --dry-run prints
the docker invocations without running them.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat, err := catalog.Default()
			if err != nil {
				return err
			}
			var cells []imageCell
			if fromFleet != "" {
				cells, err = planCellsFromFleet(cat, fromFleet, filterArch)
				if err != nil {
					return err
				}
			} else {
				cells = planCells(cat, filterOS, filterPG, filterArch)
			}
			if len(cells) == 0 {
				return fmt.Errorf("image build: no cells match the filters (--only-os=%q --only-pg=%q --only-arch=%q --from-fleet=%q)",
					filterOS, filterPG, filterArch, fromFleet)
			}
			// Pin a recipe digest into each cell now so every
			// downstream printout (tag, "[i/N] ... → tag") and
			// the actual `docker build -t TAG` see the same
			// content-addressed value.  An entrypoint edit
			// flips the digest, the tag, and forces a rebuild
			// regardless of any cache state on the host.
			for i := range cells {
				cells[i].RecipeDigest = imagetag.RecipeDigest(cells[i].Family, dfDir)
			}
			// Fail-fast: the Dockerfiles COPY bin/pg_hardstorage,
			// so a missing binary would only surface mid-way
			// through the docker build with a confusing
			// "file not found in build context" error.
			if !dryRun {
				if _, err := os.Stat("bin/pg_hardstorage"); err != nil {
					return fmt.Errorf("image build: bin/pg_hardstorage not found — run `make build` first (%w)", err)
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Building %d images (parallelism=%d, dry-run=%t)\n",
				len(cells), parallelism, dryRun)
			for i, c := range cells {
				cmdline := dockerBuildArgs(repo, dfDir, c)
				fmt.Fprintf(cmd.OutOrStdout(), "[%d/%d] %s + %s + %s → %s\n",
					i+1, len(cells), c.OS, c.PG, c.Arch, c.Tag(repo))
				if dryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "    docker %s\n", strings.Join(cmdline, " "))
					continue
				}
				if err := runDocker(cmd, cmdline); err != nil {
					return fmt.Errorf("image build %s: %w", c.Tag(repo), err)
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&repo, "repo", defaultImageRepo, "image registry repo prefix")
	c.Flags().StringVar(&dfDir, "dockerfile-dir", defaultDockerfileDir,
		"dockerfiles/testbed/ path (Dockerfiles only; the build context is always the repo root so `COPY bin/pg_hardstorage` resolves)")
	c.Flags().StringVar(&filterOS, "only-os", "", "build only cells whose OS contains this substring")
	c.Flags().StringVar(&filterPG, "only-pg", "", "build only cells with this PG version")
	c.Flags().StringVar(&filterArch, "only-arch", "", "build only cells for this architecture")
	c.Flags().StringVar(&fromFleet, "from-fleet", "",
		"build only the (os, pg, arch) tuples referenced by the supplied fleet.yaml — much faster than building the full catalog")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print docker commands without executing")
	c.Flags().IntVar(&parallelism, "parallel", 4,
		"max concurrent builds (currently sequential — flag reserved for future)")
	return c
}

// planCellsFromFleet reads fleet.yaml, dedupes the
// (os, pg, arch) tuples it references, and returns the
// matching imageCell list.  --only-arch still applies as a
// further filter on top.
func planCellsFromFleet(cat *catalog.Catalog, fleetPath, filterArch string) ([]imageCell, error) {
	f, err := config.LoadFleet(fleetPath)
	if err != nil {
		return nil, err
	}
	if err := f.Validate(cat); err != nil {
		return nil, fmt.Errorf("image build --from-fleet: %s is invalid: %w", fleetPath, err)
	}
	seen := map[string]bool{}
	var out []imageCell
	for _, e := range f.Entries {
		arch := e.EffectiveArch()
		if filterArch != "" && arch != filterArch {
			continue
		}
		key := e.OS + "|" + e.PG + "|" + arch
		if seen[key] {
			continue
		}
		seen[key] = true
		o, err := cat.FindOS(e.OS)
		if err != nil {
			return nil, err
		}
		out = append(out, imageCell{
			OS:       e.OS,
			Image:    o.EffectiveImage(),
			PG:       e.PG,
			Arch:     arch,
			Family:   o.Family,
			Packages: cat.EffectivePackages(o),
		})
	}
	return out, nil
}

func newImagePullCmd() *cobra.Command {
	var (
		repo       string
		filterOS   string
		filterPG   string
		filterArch string
	)
	c := &cobra.Command{
		Use:   "pull",
		Short: "Pull pre-built testbed images from the registry",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat, err := catalog.Default()
			if err != nil {
				return err
			}
			cells := planCells(cat, filterOS, filterPG, filterArch)
			for _, c := range cells {
				tag := c.Tag(repo)
				fmt.Fprintf(cmd.OutOrStdout(), "  pulling %s ...\n", tag)
				if err := runDocker(cmd, []string{"pull", tag}); err != nil {
					return fmt.Errorf("pull %s: %w", tag, err)
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&repo, "repo", defaultImageRepo, "image registry repo prefix")
	c.Flags().StringVar(&filterOS, "only-os", "", "pull only cells whose OS contains this substring")
	c.Flags().StringVar(&filterPG, "only-pg", "", "pull only cells with this PG version")
	c.Flags().StringVar(&filterArch, "only-arch", "", "pull only cells for this architecture")
	return c
}

func newImagePushCmd() *cobra.Command {
	var (
		repo       string
		filterOS   string
		filterPG   string
		filterArch string
	)
	c := &cobra.Command{
		Use:   "push",
		Short: "Push locally-built testbed images to the registry",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat, err := catalog.Default()
			if err != nil {
				return err
			}
			cells := planCells(cat, filterOS, filterPG, filterArch)
			for _, c := range cells {
				tag := c.Tag(repo)
				fmt.Fprintf(cmd.OutOrStdout(), "  pushing %s ...\n", tag)
				if err := runDocker(cmd, []string{"push", tag}); err != nil {
					return fmt.Errorf("push %s: %w", tag, err)
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&repo, "repo", defaultImageRepo, "image registry repo prefix")
	c.Flags().StringVar(&filterOS, "only-os", "", "push only cells whose OS contains this substring")
	c.Flags().StringVar(&filterPG, "only-pg", "", "push only cells with this PG version")
	c.Flags().StringVar(&filterArch, "only-arch", "", "push only cells for this architecture")
	return c
}

// --- helpers ----------------------------------------------------------

// imageCell is one (os, pg, arch) target the image-build pipeline
// considers.  Tag() turns it into a registry tag.
//
// OS is the operator-friendly catalog id ("opensuse:leap-15");
// Image is the actual docker pull spec ("opensuse/leap:15") that
// goes into the Dockerfile's FROM via --build-arg.  Most distros
// have OS == Image; ones with custom namespaces or non-Hub
// registries override.  Falls back to OS when the catalog has no
// override (the legacy / library-namespace path).
type imageCell struct {
	OS       string
	Image    string
	PG       string
	Arch     string
	Family   string
	Packages string

	// RecipeDigest is the imagetag.RecipeDigest output for
	// (Family, dockerfileDir).  Pinned at plan time so the
	// build driver and the tag-rendering helpers see the
	// same content even if the dockerfile/entrypoint are
	// edited mid-run.  Empty = legacy unversioned tag.
	RecipeDigest string
}

// effectiveImage returns Image when set, OS otherwise.  Used by
// dockerBuildArgs so unit tests can construct an imageCell
// without populating Image and still get a working --build-arg.
func (c imageCell) effectiveImage() string {
	if c.Image != "" {
		return c.Image
	}
	return c.OS
}

// Tag renders the registry tag for this cell — delegates to
// internal/testkit/imagetag so the compose generator and the
// build driver always agree on what to look for.
func (c imageCell) Tag(repo string) string {
	return imagetag.ForWithRecipe(repo, c.OS, c.PG, c.Arch, c.Family, c.Packages, c.RecipeDigest)
}

func imageTag(repo, osID, pg, arch string) string {
	return imageCell{OS: osID, PG: pg, Arch: arch}.Tag(repo)
}

// planCells walks the catalog and emits one imageCell per
// (OS, PG, arch) tuple, applying the filter substrings.
func planCells(cat *catalog.Catalog, filterOS, filterPG, filterArch string) []imageCell {
	var out []imageCell
	for _, o := range cat.OSes {
		if filterOS != "" && !strings.Contains(o.ID, filterOS) {
			continue
		}
		for _, pg := range o.PGVersions {
			if filterPG != "" && pg != filterPG {
				continue
			}
			for _, arch := range o.Arches {
				if filterArch != "" && arch != filterArch {
					continue
				}
				out = append(out, imageCell{
					OS:       o.ID,
					Image:    o.EffectiveImage(),
					PG:       pg,
					Arch:     arch,
					Family:   o.Family,
					Packages: cat.EffectivePackages(&o),
				})
			}
		}
	}
	return out
}

// dockerBuildArgs builds the argv for `docker build` for one
// cell.  The build context is the repo root ("."), not the
// Dockerfile dir — the Dockerfiles `COPY bin/pg_hardstorage`
// and `COPY dockerfiles/testbed/entrypoint-pg.sh`, both
// repo-root-relative.
func dockerBuildArgs(repo, dfDir string, c imageCell) []string {
	df := filepath.Join(dfDir, "Dockerfile."+c.Family+"-family")
	tag := c.Tag(repo)
	return []string{
		"build",
		"-f", df,
		"-t", tag,
		// OS_IMAGE is what the Dockerfile's FROM resolves —
		// must be a real docker pull spec.  effectiveImage()
		// applies the catalog's optional `image:` override
		// (so `opensuse:leap-15` builds against the real
		// `opensuse/leap:15`).
		"--build-arg", "OS_IMAGE=" + c.effectiveImage(),
		"--build-arg", "PG_VERSION=" + c.PG,
		"--build-arg", "PACKAGES=" + c.Packages,
		"--platform", "linux/" + c.Arch,
		".",
	}
}

// runDocker forks `docker` with the supplied args, plumbing
// stdout / stderr through to the cobra command.
func runDocker(cmd *cobra.Command, args []string) error {
	exe := exec.CommandContext(cmd.Context(), "docker", args...)
	exe.Stdout = cmd.OutOrStdout()
	exe.Stderr = cmd.ErrOrStderr()
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker not on PATH (install Docker / Podman / Colima)")
	}
	if err := exe.Run(); err != nil {
		return err
	}
	_ = os.Stdout // silence unused-import if a future refactor drops the os import
	return nil
}
