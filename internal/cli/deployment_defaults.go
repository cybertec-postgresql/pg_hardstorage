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
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// deploymentTDE resolves the effective source-TDE declaration for a backup:
// the --tde / --tde-engine / --tde-key-ref flags OR the named deployment's
// `tde:` block in pg_hardstorage.yaml. Returns nil when TDE is declared
// neither way — the manifest then carries no source_tde block, exactly the
// historical behaviour. Flags win over the config block, matching how
// deploymentDefaults / deploymentKMS treat --repo / --pg-connection / --kek.
//
// This is the wiring the tde-awareness docs promised: before it, the `tde:`
// block and any --tde flag were silently ignored on the backup path, so
// source_tde was always null even for a declared-TDE deployment (issue #48).
func deploymentTDE(deployment string, tdeFlag bool, engineFlag, keyRefFlag string) *backup.SourceTDEInfo {
	enabled := tdeFlag || engineFlag != "" || keyRefFlag != ""
	engine, keyRef := engineFlag, keyRefFlag

	// Fold in the deployment's tde: block unless the operator pinned the
	// corresponding value on the command line.
	if deployment != "" {
		if p, err := paths.Resolve(paths.DefaultOptions()); err == nil {
			if loaded, err := config.Load(p); err == nil && loaded != nil {
				if dep, ok := loaded.Config.Deployments[deployment]; ok && dep.TDE.Enabled {
					enabled = true
					if engine == "" {
						engine = dep.TDE.Engine
					}
					if keyRef == "" {
						keyRef = dep.TDE.KeyRef
					}
				}
			}
		}
	}
	if !enabled {
		return nil
	}
	// Empty engine stamps as the literal "unspecified" (matches the
	// SourceTDEInfo contract): the operator declared TDE without naming
	// an engine, which is accepted.
	if engine == "" {
		engine = "unspecified"
	}
	return &backup.SourceTDEInfo{Engine: engine, KeyRef: keyRef}
}

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

// deploymentKMS resolves the key material for a deployment-scoped
// command: the KEK reference and the provider config map that opens it.
//
// Precedence mirrors deploymentDefaults — an explicit --kek / --kms-config
// on the command line always wins, and the deployment's `kek_ref` (plus the
// matching top-level `kms.providers` entry) fills the gap. Without this,
// cloud KMS could only be driven by repeating the flags on every
// invocation, which no scheduled path can do (issue #44).
//
// Config-load failures are non-fatal for the same reason they are in
// deploymentDefaults: the caller ends up exactly where it would have been
// without a config file, rather than failing on an unrelated parse error.
func deploymentKMS(deployment, kekRef string, kmsCfg map[string]string) (string, map[string]any) {
	// An explicit --kms-config with no --kek still applies to a
	// configured kek_ref, so we only short-circuit when the operator
	// pinned both.
	if kekRef != "" && len(kmsCfg) > 0 {
		return kekRef, stringMapToAny(kmsCfg)
	}
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return kekRef, stringMapToAny(kmsCfg)
	}
	loaded, err := config.Load(p)
	if err != nil || loaded == nil {
		return kekRef, stringMapToAny(kmsCfg)
	}
	return resolveDeploymentKMS(deployment, kekRef, kmsCfg, loaded.Config)
}

// resolveDeploymentKMS is deploymentKMS's pure core, split out so tests
// can drive it from an in-memory config.
func resolveDeploymentKMS(deployment, kekRef string, kmsCfg map[string]string, cfg config.Config) (string, map[string]any) {
	if kekRef == "" && deployment != "" {
		if dep, ok := cfg.Deployments[deployment]; ok {
			kekRef = dep.KEKRef
		}
	}
	if len(kmsCfg) > 0 {
		// Flag config is authoritative when present — an operator
		// overriding region on one run shouldn't silently inherit half
		// the file's settings.
		return kekRef, stringMapToAny(kmsCfg)
	}
	return kekRef, cfg.KMSProviderConfig(kekRef)
}

// deploymentKMSResolver is the read-side counterpart of deploymentKMS,
// for restore / verify / partial. Those commands don't choose a KEK —
// the manifest already names one — so what they need is the provider
// settings for whatever ref they encounter.
//
// The returned func is keyed by KEKRef rather than fixed to one config
// because a repo can legitimately hold manifests under several refs at
// once (mid-rotation, or one repo shared by deployments with different
// KEKs). An explicit --kms-config still overrides everything.
//
// The config is loaded once, when the resolver is built; a load failure
// degrades to flags-only, matching deploymentDefaults.
func deploymentKMSResolver(kmsCfg map[string]string) func(kekRef string) map[string]any {
	if len(kmsCfg) > 0 {
		flags := stringMapToAny(kmsCfg)
		return func(string) map[string]any { return flags }
	}
	var declared config.KMSConfig
	if p, err := paths.Resolve(paths.DefaultOptions()); err == nil {
		if loaded, err := config.Load(p); err == nil && loaded != nil {
			declared = loaded.Config.KMS
		}
	}
	return declared.ProviderConfig
}

// resolveDeploymentDefaultsPreRun is the root PersistentPreRunE handler
// (added to the chain in NewRoot) that fills an unset --repo /
// --pg-connection from the deployment named as the command's first
// positional argument, *before* Cobra validates required flags. This
// makes every deployment-scoped command honour the deployment catalogue
// in pg_hardstorage.yaml (#12) instead of demanding flags a configured
// deployment already declares.
//
// It is deliberately conservative: it acts only when the command has the
// flag, the flag is unset on the command line, and the first argument
// names a known deployment. A command whose first argument is not a
// deployment sees a lookup miss and is left untouched, so it still errors
// exactly as before. Path/config-load failures are non-fatal — Cobra's
// required-flag check then fires just as it did previously.
func resolveDeploymentDefaultsPreRun(cmd *cobra.Command, args []string) error {
	fl := cmd.Flags()
	repoF := fl.Lookup("repo")
	pgF := fl.Lookup("pg-connection")
	repoNeed := repoF != nil && !repoF.Changed
	pgNeed := pgF != nil && !pgF.Changed
	if !repoNeed && !pgNeed {
		return nil
	}
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return nil
	}
	loaded, err := config.Load(p)
	if err != nil {
		return nil
	}
	if len(args) == 0 {
		// All-deployments mode (bare `status`, `rotate`, `audit
		// anchor`, ...): there is no positional to key on, but when
		// every configured deployment points at the SAME repository
		// the answer is unambiguous — use it. `status db1` already
		// worked flag-free; bare `status` demanding --repo the config
		// plainly knows was maddening. With multiple distinct repos
		// we stay hands-off and the required-flag error fires.
		if repoNeed {
			if repo, ok := soleConfiguredRepo(loaded.Config.Deployments); ok {
				_ = fl.Set("repo", repo)
			}
		}
		return nil
	}
	dep, ok := loaded.Config.Deployments[args[0]]
	if !ok {
		return nil
	}
	if repoNeed && dep.Repo != "" {
		_ = fl.Set("repo", dep.Repo)
	}
	if pgNeed && dep.PGConnection != "" {
		_ = fl.Set("pg-connection", dep.PGConnection)
	}
	return nil
}

// soleConfiguredRepo returns the repo URL shared by every configured
// deployment, or ok=false when there are none or they disagree.
func soleConfiguredRepo(deps map[string]config.DeploymentConfig) (string, bool) {
	repo := ""
	for _, d := range deps {
		if d.Repo == "" {
			continue
		}
		if repo == "" {
			repo = d.Repo
			continue
		}
		if d.Repo != repo {
			return "", false
		}
	}
	return repo, repo != ""
}

// requireBackupIDArg rejects an empty/whitespace backup-ID positional as a
// usage error. Without it, `verify db1 ""` (the classic unset-shell-variable
// slip) fell through to the manifest read and was reported as a manifest
// SIGNATURE failure (exit 9 — the "backup corrupt/tampered" code that pages
// people), and `show db1 ""` as an internal error (exit 1). An empty
// argument is operator input, not corruption.
func requireBackupIDArg(command, id string) error {
	if strings.TrimSpace(id) == "" {
		return output.NewError("usage.bad_args",
			command+": backup ID must not be empty (use `latest` for the newest backup)").
			Wrap(output.ErrUsage)
	}
	return nil
}
