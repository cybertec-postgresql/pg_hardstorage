// compose.go — `compose generate` subcommand: renders a fleet.yaml into docker-compose.yaml.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/catalog"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/compose"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
)

func newComposeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "compose <generate>",
		Short: "Generate docker-compose YAML from a fleet",
	}
	c.AddCommand(newComposeGenerateCmd())
	return c
}

func newComposeGenerateCmd() *cobra.Command {
	var (
		fleetPath     string
		outPath       string
		project       string
		imageRepo     string
		hostPort      int
		hostRepoDir   string
		dockerfileDir string
		noToxiproxy   bool
		noVolume      bool
		force         bool
	)
	c := &cobra.Command{
		Use:   "generate",
		Short: "Render a fleet.yaml as docker-compose.yaml",
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := config.LoadFleet(fleetPath)
			if err != nil {
				return err
			}
			cat, err := catalog.Default()
			if err != nil {
				return err
			}
			if err := f.Validate(cat); err != nil {
				return err
			}
			out, err := compose.Generate(f, cat, compose.Options{
				ProjectName:       project,
				ImageRepo:         imageRepo,
				HostPortBase:      hostPort,
				HostRepoDir:       hostRepoDir,
				DockerfileDir:     dockerfileDir,
				IncludeToxiproxy:  !noToxiproxy,
				IncludeRepoVolume: !noVolume,
			})
			if err != nil {
				return err
			}
			if outPath == "" || outPath == "-" {
				_, err := fmt.Fprint(cmd.OutOrStdout(), out)
				return err
			}
			if !force && existsOnDisk(outPath) {
				return fmt.Errorf("compose generate: %s exists (pass --force to overwrite)", outPath)
			}
			if err := os.WriteFile(outPath, []byte(out), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"✓ wrote %s (%d bytes, %d entries)\n", outPath, len(out), len(f.Entries))
			return nil
		},
	}
	c.Flags().StringVar(&fleetPath, "fleet", defaultFleetPath, "fleet YAML input")
	c.Flags().StringVar(&outPath, "out", "", "docker-compose output path (default: stdout)")
	c.Flags().StringVar(&project, "project", "pgvalidate", "docker-compose project name")
	c.Flags().StringVar(&imageRepo, "image-repo", "", "image registry repo prefix (default: ghcr.io path)")
	c.Flags().IntVar(&hostPort, "host-port-base", 15432, "first host port for PG mapping")
	c.Flags().StringVar(&hostRepoDir, "host-repo-dir", "./repo-data",
		"host path bind-mounted into every container as /var/lib/pg_hardstorage/repo")
	c.Flags().StringVar(&dockerfileDir, "dockerfile-dir", defaultDockerfileDir,
		"directory holding the testbed Dockerfiles + entrypoint-pg.sh; the recipe content is hashed into the image tag so an entrypoint fix forces a rebuild instead of reusing a stale local image")
	c.Flags().BoolVar(&noToxiproxy, "no-toxiproxy", false,
		"omit per-cell toxiproxy services")
	c.Flags().BoolVar(&noVolume, "no-volume", false,
		"omit the shared repo-data volume (e.g. when using S3)")
	c.Flags().BoolVar(&force, "force", false, "overwrite existing file")
	return c
}
