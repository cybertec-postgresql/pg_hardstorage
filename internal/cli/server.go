// server.go — CLI surface for running the control-plane HTTP server.
package cli

import (
	"fmt"
	"io"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// newRealServerCmd implements `pg_hardstorage server`. The control
// plane long-lived process. v0.1 ships the read-mostly cut described
// in internal/server: REST API on a configurable address, mTLS +
// bearer-token auth, an in-memory agent registry, repo-walking
// handlers for deployments + backups.
//
// Lifecycle:
//   - On start, the listener binds and serves immediately.
//   - SIGINT / SIGTERM trigger a graceful shutdown (10s grace).
//   - The process exits cleanly (rc 0) on signal-induced shutdown,
//     non-zero on listener bring-up failure.
//
// Operators wire this into systemd via the
// pg_hardstorage-server.service unit shipped under
// deploy/systemd/ (added in a follow-up; v0.1 ships the binary
// surface and operator-supplied invocation).
func newRealServerCmd() *cobra.Command {
	var (
		listen            string
		repoFlag          []string
		certFile          string
		keyFile           string
		clientCAFile      string
		tokenFile         string
		coordBackend      string
		coordDSN          string
		maxConcurrentJobs int
	)
	c := &cobra.Command{
		Use:   "server",
		Short: "Run the control plane",
		Long: `Run the pg_hardstorage control plane.

v0.1 cut: HTTP/REST listener with mTLS + bearer-token auth, an
in-memory agent registry, repo-walking handlers for deployments +
backups. adds the PostgreSQL-backed job registry: pass
--coord-backend pg --coord-dsn 'postgres://...' to persist dispatch
state across restarts and to run multiple control planes against the
same database (FOR UPDATE SKIP LOCKED gives atomic claims).

The control plane never uses an embedded database (no SQLite, no
BoltDB). State is either in-memory (default) or in a PostgreSQL the
operator already runs.

Endpoints (all under /v1/):

  GET  /healthz                            (no auth)
  GET  /readyz                             (no auth; per-repo reachability)
  GET  /version
  GET  /deployments
  GET  /deployments/<name>/backups
  GET  /agents [?include_inactive=true]
  POST /agents/heartbeat

Server runtime settings are currently flag-only. The global --config file
continues to configure deployments, repositories, sinks, and paths, but it
does not define a server: block.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := server.Config{
				Listen:            listen,
				Repos:             repoFlag,
				MaxConcurrentJobs: maxConcurrentJobs,
				TLS: server.TLSConfig{
					CertFile:     certFile,
					KeyFile:      keyFile,
					ClientCAFile: clientCAFile,
				},
				Auth: server.AuthConfig{
					TokenFile: tokenFile,
				},
			}

			// Build the job registry per --coord-backend. Default is
			// memory (matches v0.4 behaviour); pg switches to the PG
			// backend so dispatch state survives a restart and multiple
			// control planes can claim atomically.
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			var jobs *server.JobRegistry
			switch strings.ToLower(coordBackend) {
			case "", "memory":
				jobs = server.NewJobRegistry()
			case "pg", "postgres", "postgresql":
				if coordDSN == "" {
					return output.NewError("server.coord_dsn_missing",
						"--coord-backend pg requires --coord-dsn (postgres://...)")
				}
				backend, err := server.OpenPGBackend(ctx, coordDSN)
				if err != nil {
					return output.NewError("server.coord_open_failed",
						fmt.Sprintf("server: open pg backend: %v", err)).Wrap(err)
				}
				jobs = server.NewJobRegistryWithBackend(backend)
			default:
				return output.NewError("server.coord_backend_unknown",
					fmt.Sprintf("server: unknown coord backend %q (want memory|pg)", coordBackend))
			}

			s, err := server.NewWithJobs(cfg, jobs)
			if err != nil {
				return output.NewError("server.new_failed",
					fmt.Sprintf("server: %v", err)).Wrap(err)
			}
			defer func() { _ = s.Close() }()

			if err := s.Run(ctx); err != nil {
				return output.NewError("server.run_failed",
					fmt.Sprintf("server: %v", err)).Wrap(err)
			}
			d := DispatcherFrom(cmd)
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(serverResultBody{
				Listen: s.Listen(),
			}))
		},
	}
	c.Flags().StringVar(&listen, "listen", "127.0.0.1:8443",
		"bind address (host:port)")
	c.Flags().StringArrayVar(&repoFlag, "repo", nil,
		"repository URL to index (repeatable)")
	c.Flags().StringVar(&certFile, "tls-cert", "",
		"TLS server certificate (PEM)")
	c.Flags().StringVar(&keyFile, "tls-key", "",
		"TLS server key (PEM)")
	c.Flags().StringVar(&clientCAFile, "client-ca", "",
		"client CA bundle (enables mTLS with verify)")
	c.Flags().StringVar(&tokenFile, "token-file", "",
		"file containing the bearer token (single-line)")
	c.Flags().StringVar(&coordBackend, "coord-backend", "memory",
		"coordination backend for jobs: memory (default, ephemeral) | pg (persistent, multi-instance HA)")
	c.Flags().StringVar(&coordDSN, "coord-dsn", "",
		"PostgreSQL DSN when --coord-backend pg (postgres://user:pass@host/db?sslmode=...)")
	c.Flags().IntVar(&maxConcurrentJobs, "max-concurrent-jobs", 0,
		"cap on jobs running at once across the fleet (0 = unlimited; backpressure so a burst can't storm storage/PostgreSQL). For multi-control-plane HA set the same value on every instance.")
	return c
}

type serverResultBody struct {
	Listen string `json:"listen"`
}

// WriteText renders the clean-shutdown confirmation as human-readable text to w.
func (b serverResultBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ control plane stopped cleanly\n  Listen: %s\n", b.Listen)
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
