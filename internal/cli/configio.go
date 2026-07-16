// configio.go — config-file read/write helpers shared by notify/schedule/deployment commands.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// configFilePath is the canonical path the read/write helpers use.
// Same lookup as the rest of the CLI: paths.Resolve handles the
// XDG / FHS / env / --config precedence; we just append the
// well-known filename here.
// captureConfigFlag forwards the global -c/--config flag to the paths
// layer via PG_HARDSTORAGE_CONFIG_FILE, so every config.Load call in
// this process (all 40-odd, plus write-back) honours it uniformly.
// Must run BEFORE installDispatcher / resolveDeploymentDefaults, which
// both load config. The flag was previously registered but read
// nowhere — a silent no-op.
func captureConfigFlag(cmd *cobra.Command, _ []string) error {
	cfgFile, _ := cmd.Flags().GetString("config")
	if strings.TrimSpace(cfgFile) != "" {
		_ = os.Setenv("PG_HARDSTORAGE_CONFIG_FILE", cfgFile)
	}
	return nil
}

func configFilePath(p *paths.Paths) string {
	// Honour an explicit -c/--config file for write-back too, so
	// `deployment edit -c staging.yaml` mutates staging.yaml rather
	// than silently rewriting the XDG/FHS default.
	if override := p.ConfigFileOverride; override != "" {
		return override
	}
	return filepath.Join(p.Config.Value, "pg_hardstorage.yaml")
}

// loadEditableConfig reads the merged config exactly once and returns
// (paths, current config, write-back closure). The closure serialises
// the config back to disk, preserving the schema header. Drop-in
// files (`conf.d/*.yaml`) are READ but never written — write-back
// always targets the main file. Operators who want their changes
// in a drop-in must edit the YAML by hand.
//
// This shape is what the notify / schedule / deployment commands
// share: they each load, mutate the in-memory Config, and call the
// closure to persist. Centralising the I/O here keeps the
// mutation paths free of file-system concerns.
func loadEditableConfig() (*paths.Paths, *config.Config, func(*config.Config) error, error) {
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return nil, nil, nil, output.NewError("internal", err.Error()).Wrap(err)
	}
	loaded, err := config.Load(p)
	if err != nil {
		return nil, nil, nil, output.NewError("config.load_failed",
			fmt.Sprintf("config: load: %v", err)).Wrap(err)
	}
	cfg := config.Config{}
	if loaded != nil {
		cfg = loaded.Config
	}
	if cfg.Schema == "" {
		cfg.Schema = config.Schema
	}

	write := func(updated *config.Config) error {
		body, err := config.Marshal(updated)
		if err != nil {
			return output.NewError("config.marshal_failed", err.Error()).Wrap(err)
		}
		path := configFilePath(p)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return output.NewError("config.mkdir_failed",
				fmt.Sprintf("config: mkdir %s: %v", filepath.Dir(path), err)).Wrap(err)
		}
		// fsutil.WriteFileAtomic: another process (or a parallel
		// `pg_hardstorage` invocation) could be reading the config
		// concurrently — atomic rewrite avoids tearing the YAML.
		if err := fsutil.WriteFileAtomic(path, body, 0o600); err != nil {
			return output.NewError("config.write_failed",
				fmt.Sprintf("config: write %s: %v", path, err)).Wrap(err)
		}
		return nil
	}
	return p, &cfg, write, nil
}

// mustHaveDeployment returns the deployment matching name from cfg,
// or a structured error with code "notfound.deployment". Used by
// the mutating sub-commands so the error path is consistent.
func mustHaveDeployment(cfg *config.Config, name string) (config.DeploymentConfig, error) {
	if cfg.Deployments == nil {
		return config.DeploymentConfig{}, output.NewError("notfound.deployment",
			fmt.Sprintf("config: no such deployment %q (config has no deployments yet)", name))
	}
	dep, ok := cfg.Deployments[name]
	if !ok {
		return config.DeploymentConfig{}, output.NewError("notfound.deployment",
			fmt.Sprintf("config: no such deployment %q", name))
	}
	return dep, nil
}
