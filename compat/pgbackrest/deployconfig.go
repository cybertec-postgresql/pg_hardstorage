// deployconfig.go — stanza → pg_hardstorage.yaml deployment fallback.
//
// pgBackRest keeps the connection and repository for a stanza in
// pgbackrest.conf, so real cron lines carry only the stanza:
//
//	pgbackrest --stanza=db1 backup
//
// The shim's flag translator can only build --pg-connection / --repo
// from the pg1-* / repo1-* flags, so that canonical invocation failed
// with `usage.missing_flag: backup: --pg-connection, --repo are
// required` — contradicting the migration guide's promise that an
// existing cron "runs unchanged" after `compat translate`. The barman
// shim already did the equivalent lookup (deployconfig.go there); this
// is the pgBackRest arm of the same idea.
//
// Precedence is explicit-flags-win: anything the operator passed on
// the command line is authoritative, and the config is consulted only
// for what is still missing. That keeps a half-specified invocation
// (`--stanza=db1 --pg1-host=... backup`, repo in the config) working
// as well as the fully-implicit one.
package pgbackrest

import (
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// stanzaSettings is the slice of deployment config the shim can
// substitute for missing pg1-* / repo1-* flags.
type stanzaSettings struct {
	Repo         string
	PGConnection string
}

// stanzaLookup resolves a stanza name against pg_hardstorage.yaml,
// using the same path precedence the native CLI uses. A missing
// config file or a missing deployment is NOT an error here: the
// operator may legitimately be passing every value on the command
// line. Callers treat the zero value as "nothing to add".
//
// Tests replace this via swapStanzaLookup.
var stanzaLookup = func(stanza string) stanzaSettings {
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return stanzaSettings{}
	}
	loaded, err := config.Load(p)
	if err != nil || loaded == nil {
		return stanzaSettings{}
	}
	dep, ok := loaded.Config.Deployments[stanza]
	if !ok {
		return stanzaSettings{}
	}
	return stanzaSettings{Repo: dep.Repo, PGConnection: dep.PGConnection}
}

// swapStanzaLookup temporarily replaces the stanza lookup and returns
// a restore closure, mirroring swapDispatcher's shape.
func swapStanzaLookup(f func(string) stanzaSettings) func() {
	prev := stanzaLookup
	stanzaLookup = f
	return func() { stanzaLookup = prev }
}

// missingStanzaHint is the remediation attached when neither the
// command line nor the config could supply what a verb needs. It names
// both ways out so an operator mid-migration is not left guessing.
func missingStanzaHint(stanza string, missing []string) error {
	return fmt.Errorf(
		"pg-hardstorage-pgbackrest: --stanza=%s: no %v on the command line and no `deployments.%s` in pg_hardstorage.yaml — "+
			"convert your pgbackrest.conf once with `pg_hardstorage compat translate --from pgbackrest <config-path> --out-file <pg_hardstorage.yaml>`, "+
			"or pass the pg1-*/repo1-* flags explicitly",
		stanza, missing, stanza)
}
