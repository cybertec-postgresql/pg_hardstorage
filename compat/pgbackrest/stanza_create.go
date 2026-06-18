// stanza_create.go — pgBackRest shim verb: `pgbackrest stanza-create` → native `repo init <url>`.
package pgbackrest

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newStanzaCreateCmd implements `pgbackrest --stanza=<n> stanza-create`.
//
// Native equivalent: `pg_hardstorage repo init <repo-url>`
// followed by writing a deployment YAML stub.  We delegate
// the repo-init half to the native CLI; the deployment YAML
// stub is the operator's responsibility (the translator
// subcommand emits a complete pg_hardstorage.yaml from the
// pgbackrest.conf — see internal/cli/compat_translate.go).
func newStanzaCreateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:           "stanza-create",
		Short:         "Initialise the repository and deployment scaffold",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStanzaCreate(globalArgs)
		},
	}
	return c
}

func runStanzaCreate(a pgbackrestArgs) error {
	if a.stanza == "" {
		return fmt.Errorf("pg-hardstorage-pgbackrest: stanza-create: --stanza is required")
	}
	repoURL, warnings, err := buildRepoURL(a)
	if err != nil {
		return err
	}
	if repoURL == "" {
		return fmt.Errorf(
			"pg-hardstorage-pgbackrest: stanza-create: --repo1-path or --repo1-s3-bucket is required")
	}
	emitWarnings(warnings)

	// pgBackRest's stanza-create is roughly two operations:
	//   1. Initialise the repo if it isn't already.
	//   2. Stamp a per-stanza directory in the repo.
	// Native has #1 as a first-class verb.  #2 is a no-op for
	// pg_hardstorage (deployments are scoped at manifest
	// metadata, not repo directory layout).
	args := []string{"repo", "init", repoURL}
	if rc := dispatchNative(args); rc != 0 {
		return fmt.Errorf("pg-hardstorage-pgbackrest: stanza-create: repo init failed (exit %d)", rc)
	}

	// Hint operators that the YAML stub still needs writing.
	fmt.Fprintln(stderrWriter,
		"pg-hardstorage-pgbackrest: stanza-create: repository initialised. "+
			"Run `pg_hardstorage compat translate --from pgbackrest /etc/pgbackrest/pgbackrest.conf` "+
			"to generate pg_hardstorage.yaml.")
	return nil
}
