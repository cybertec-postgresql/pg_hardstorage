// deployment_defaults.go — shared resolution of connection/repo defaults
// from the deployment catalogue in pg_hardstorage.yaml.
//
// Commands that take a <deployment> argument (backup, restore, verify, …)
// should let the operator omit --pg-connection / --repo when the named
// deployment already declares them in config. This is the single place
// that resolves those defaults so every command behaves identically and
// no command silently demands flags a configured deployment already has
// (issue #12).
package cli

import (
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// resolveDeploymentDefaults fills empty pgConn / repoURL from the named
// deployment in deps. Explicit (non-empty) values always win, so a flag
// passed on the command line overrides the configured value. A deployment
// that isn't in deps (or empty fields) leaves the inputs untouched.
func resolveDeploymentDefaults(deployment, pgConn, repoURL string, deps map[string]config.DeploymentConfig) (string, string) {
	if deployment == "" {
		return pgConn, repoURL
	}
	dep, ok := deps[deployment]
	if !ok {
		return pgConn, repoURL
	}
	if pgConn == "" {
		pgConn = dep.PGConnection
	}
	if repoURL == "" {
		repoURL = dep.Repo
	}
	return pgConn, repoURL
}

// deploymentDefaults loads the on-disk config and applies
// resolveDeploymentDefaults. It short-circuits when there is nothing to
// resolve. Path/config-load errors are deliberately non-fatal: the
// caller's required-flag check still fires if nothing was resolved, so a
// missing config degrades to the same "flag is required" error as before
// rather than a confusing load failure.
func deploymentDefaults(deployment, pgConn, repoURL string) (string, string) {
	if deployment == "" || (pgConn != "" && repoURL != "") {
		return pgConn, repoURL
	}
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return pgConn, repoURL
	}
	loaded, err := config.Load(p)
	if err != nil {
		return pgConn, repoURL
	}
	return resolveDeploymentDefaults(deployment, pgConn, repoURL, loaded.Config.Deployments)
}
