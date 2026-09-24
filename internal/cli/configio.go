// configio.go — config-file read/write helpers shared by notify/schedule/deployment commands.
package cli

import (
	"errors"
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

// loadEditableConfig reads the config exactly once and returns
// (paths, merged config, write-back closure). The returned config is the
// merged view every reader sees (env YAML + main file + conf.d
// drop-ins), so list/show commands keep working unchanged.
//
// The closure does NOT serialise that merged view. It diffs the edited
// config against what was loaded and applies only the changes to the
// target file's own content (the -c file, else the main file) — see
// config.EditView. Persisting the merge copied every drop-in deployment
// and the PG_HARDSTORAGE_CONFIG env YAML, credentials included, into the
// main file on the first edit. Editing or removing something a drop-in
// or the env YAML defines is refused (config.defined_elsewhere, exit 2)
// with the file to edit instead; removing a drop-in deployment used to
// report "removed" while the drop-in kept defining it.
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
	view, err := config.LoadForEdit(p)
	if err != nil {
		return nil, nil, nil, output.NewError("config.load_failed",
			fmt.Sprintf("config: load: %v", err)).Wrap(err)
	}
	cfg := view.Merged
	if cfg.Schema == "" {
		cfg.Schema = config.Schema
	}

	write := func(updated *config.Config) error {
		own, err := view.Apply(updated)
		if err != nil {
			return configEditError(err)
		}
		return writeConfigFile(view.Path, own)
	}
	return p, &cfg, write, nil
}

// configEditError maps a refused layered edit onto the structured error
// operators see; anything else is an internal failure.
func configEditError(err error) error {
	var elsewhere *config.DefinedElsewhereError
	if errors.As(err, &elsewhere) {
		return output.NewError("config.defined_elsewhere", err.Error()).
			WithSuggestion(&output.Suggestion{
				Human: fmt.Sprintf("edit %s directly, or pass -c <file> to work on a single file", strings.Join(elsewhere.Sources, " / ")),
			}).Wrap(output.ErrUsage)
	}
	return output.NewError("config.marshal_failed", err.Error()).Wrap(err)
}

// writeConfigFile serialises cfg to path atomically.
func writeConfigFile(path string, cfg *config.Config) error {
	body, err := config.Marshal(cfg)
	if err != nil {
		return output.NewError("config.marshal_failed", err.Error()).Wrap(err)
	}
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
