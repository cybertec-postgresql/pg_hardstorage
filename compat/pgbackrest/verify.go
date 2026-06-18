// verify.go — pgBackRest shim verb: `pgbackrest verify [--full]` → native `pg_hardstorage verify <stanza> latest`.
package pgbackrest

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newVerifyCmd implements `pgbackrest --stanza=<n> verify`.
//
// Native dispatch: `pg_hardstorage verify <stanza> latest`.
// pgBackRest's verify checks every backup + every WAL segment
// against the manifest checksums; native's default verify
// covers the latest backup's manifest signature plus the
// SHA-256 round-trip of every chunk.  Add `--full` to spin a
// pg_verifybackup sandbox (Docker required).
func newVerifyCmd() *cobra.Command {
	var full bool
	c := &cobra.Command{
		Use:           "verify",
		Short:         "Verify the latest backup's signature and chunks",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVerify(globalArgs, full)
		},
	}
	c.Flags().BoolVar(&full, "full", false,
		"perform a sandbox restore + pg_verifybackup (Docker required)")
	return c
}

func runVerify(a pgbackrestArgs, full bool) error {
	native, warnings, err := mapToNativeArgs("verify", a)
	if err != nil {
		return err
	}
	emitWarnings(warnings)

	out := []string{native[0], a.stanza, "latest"}
	out = append(out, native[1:]...)
	if full {
		out = append(out, "--full")
	}

	if rc := dispatchNative(out); rc != 0 {
		return fmt.Errorf("pg-hardstorage-pgbackrest: verify: native CLI exited %d", rc)
	}
	return nil
}
