// pg_hardstorage_simple is the kind-interface companion binary to
// pg_hardstorage.  It exposes exactly six operations through an
// interactive prompt — no flags, no subcommands.  Operators who
// want a flag-driven CLI run pg_hardstorage directly.
//
// Design rationale and the operation list live at
// /tmp/suggest.md in the original review and at
// docs/tutorials/getting-started-simple.md once docs land.
//
// This main does the bare minimum: parse --help / --version, build
// the Env (resolved paths, merged config, cached state), hand off to
// internal/simple.Run for the menu loop.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/simple"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/simple/prompt"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/simple/state"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/version"
)

func main() {
	// Two — and only two — accepted flags.  Anything else is a
	// usage error.  Rolling our own parse keeps the binary's "no
	// flags" promise honest (no Cobra, no pflag, no surprise
	// flag-completion files generated under conf.d).
	for _, a := range os.Args[1:] {
		switch a {
		case "--help", "-h":
			printHelp()
			return
		case "--version", "-V":
			fmt.Println(version.Version)
			return
		default:
			fmt.Fprintf(os.Stderr, "pg_hardstorage_simple: unknown argument %q\n\n", a)
			printHelp()
			os.Exit(2)
		}
	}

	// SIGINT during a long-running flow (a backup, a wal stream)
	// cancels the context so the runner unwinds cleanly instead
	// of being SIGKILLed mid-write.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		fail("resolve paths: %v", err)
	}
	loaded, err := config.Load(p)
	if err != nil {
		// Config-load failure is recoverable here — first-run
		// operators have no config yet and need to reach #1.
		// Surface a one-line warning and continue with an empty
		// LoadResult so the menu still works.
		fmt.Fprintf(os.Stderr, "warn: config load: %v (continuing with empty config)\n", err)
		loaded = &config.LoadResult{}
	}
	st, err := state.Load(p.Config.Value)
	if err != nil {
		fail("load state: %v", err)
	}

	env := &simple.Env{
		Prompter: prompt.NewPrompter(),
		Paths:    p,
		Config:   loaded,
		State:    st,
	}
	if err := simple.Run(ctx, env); err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "pg_hardstorage_simple: "+format+"\n", args...)
	os.Exit(1)
}

func printHelp() {
	fmt.Print(`pg_hardstorage_simple — interactive backup helper.

Run with no arguments.  A numbered menu appears; pick an option,
answer the prompts, you're done.  There are no command-line
options — the simple binary's whole promise is that it stays
simple, and a flag-driven scripting surface is what turns a kind
interface back into a cockpit.

For automation, cron, k8s, or anything else that needs flags,
use the pg_hardstorage binary directly.

Recognised arguments:
  --help, -h      print this message
  --version, -V   print the version string
`)
}
