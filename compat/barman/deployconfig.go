// Package barman — deployment-config lookup helper.
//
// Barman's CLI surface identifies a deployment by name only:
// `barman backup db1`, `barman recover db1 ...`,
// `barman-wal-archive db1 %p`.  None of the shim verbs accept a
// `--repo` / `--pg-connection` flag — operators run them from
// cron / archive_command and trust the shim to find the
// repository the way native pgBackRest finds its `--repo1-path`
// from `pgbackrest.conf`.  The shim therefore loads
// `pg_hardstorage.yaml` (XDG / FHS / env, same precedence the
// native CLI uses) and injects `--repo` / `--pg-connection`
// derived from `deployments.<server>` into every native argv.
//
// Without this lookup the shim emitted argvs the native CLI
// rejected with `usage.missing_flag` — exposed when the e2e
// barman compat test surfaced bug B (the silent dispatch fix
// being the matching bug A).
package barman

import (
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// deploymentSettings is the slice of deployment config the
// Barman shim cares about.  Kept narrow (the shim doesn't need
// every DeploymentConfig field) so the lookup helper stays
// trivially testable.
type deploymentSettings struct {
	Repo         string
	PGConnection string
}

// deploymentLookup returns the repo + pg_connection configured
// for the named server, or an error explaining what was missing.
// The `_` second result lets the lookup be swapped in tests
// without exposing the raw config types.
//
// Resolution mirrors the native CLI: paths.Resolve picks the
// XDG / FHS / PG_HARDSTORAGE_CONFIG_DIR config directory,
// config.Load reads pg_hardstorage.yaml + conf.d/*.yaml, and
// we look up `deployments[<server>]`.
//
// Tests overwrite via swapDeploymentLookup so they can drive
// Barman shim verbs without writing a real yaml file.
var deploymentLookup = func(server string) (deploymentSettings, error) {
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return deploymentSettings{}, fmt.Errorf("resolve config paths: %w", err)
	}
	loaded, err := config.Load(p)
	if err != nil {
		return deploymentSettings{}, fmt.Errorf("load config: %w", err)
	}
	dep, ok := loaded.Config.Deployments[server]
	if !ok {
		return deploymentSettings{}, fmt.Errorf(
			"server %q not in pg_hardstorage.yaml deployments (config dir: %s) — translate barman.conf with `pg_hardstorage compat translate-barman` or add the deployment manually",
			server, p.Config.Value)
	}
	out := deploymentSettings{Repo: dep.Repo, PGConnection: dep.PGConnection}
	if out.Repo == "" {
		return out, fmt.Errorf("deployment %q has no `repo:` set in pg_hardstorage.yaml", server)
	}
	return out, nil
}

// swapDeploymentLookup temporarily replaces the deployment
// config lookup; the returned closure restores the previous
// value (deferable from tests).  Mirrors swapDispatcher's
// pattern so test setup feels familiar.
func swapDeploymentLookup(f func(string) (deploymentSettings, error)) func() {
	prev := deploymentLookup
	deploymentLookup = f
	return func() { deploymentLookup = prev }
}

// injectDeploymentFlags appends --repo (always) and
// --pg-connection (when wantsPG is true) to native, returning
// the augmented slice.  Repo is required by every native verb
// the shim dispatches; pg_connection is gated because the
// native `list` / `show` / `backup delete` commands reject the
// flag as unknown — only `backup`, `restore`, `wal push`,
// `doctor` accept it.
func injectDeploymentFlags(native []string, server string, wantsPG bool) ([]string, error) {
	dep, err := deploymentLookup(server)
	if err != nil {
		return nil, err
	}
	out := append([]string(nil), native...)
	out = append(out, "--repo", dep.Repo)
	if wantsPG {
		if dep.PGConnection == "" {
			return nil, fmt.Errorf(
				"deployment %q has no `pg_connection:` set in pg_hardstorage.yaml; verb requires a libpq DSN",
				server)
		}
		out = append(out, "--pg-connection", dep.PGConnection)
	}
	return out, nil
}
