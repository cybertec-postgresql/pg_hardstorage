// pg_hardstorage — PostgreSQL backup, done right.
//
// Entry point. Everything lives in internal/cli; main is a 3-line shim
// that returns the exit code from cli.Execute() per the v1 contract.
package main

import (
	"os"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
