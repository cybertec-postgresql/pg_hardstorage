// Package simple is the implementation surface of pg_hardstorage_simple.
//
// The binary at cmd/pg_hardstorage_simple is a thin shell over the
// dispatch loop in this package.  Each operation lives in its own
// flow_*.go file; the menu in dispatch.go routes by index.  Flows
// import the same internal/runner / repo / config packages the full
// CLI uses — there is no shell-out, no JSON parsing in between, just
// Go calls into the existing library code with prompted inputs.
package simple

import (
	"context"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/simple/prompt"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/simple/state"
)

// Env is the per-session bundle passed to every flow.  Holds the
// Prompter (UI), the resolved Paths (config / keyring / state dirs),
// the merged Config (deployment list), and the cached State (last-
// picked defaults).
//
// Flows mutate State.LastFoo as they go; the dispatch loop saves it
// after each successful flow.  An aborted flow's partial mutations
// are not persisted — failed setup doesn't leave a stale "use this
// connection" default behind.
type Env struct {
	Prompter *prompt.Prompter
	Paths    *paths.Paths
	Config   *config.LoadResult
	State    *state.State
}

// Flow is the interface every operation implements.  Pure verb shape:
// "given the Env, do your thing and return when finished or errored."
// A prompt.ErrQuit return from any flow is the normal "operator quit"
// signal and unwinds the menu cleanly.
type Flow interface {
	Name() string
	Run(ctx context.Context, env *Env) error
}
