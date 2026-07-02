// Package server implements the pg_hardstorage control plane.
//
// v0.1 is the read-mostly cut: a structurally-complete HTTP/REST
// listener with mTLS + bearer-token auth, a small set of real
// handlers (healthz, readyz, version, deployments, backups, agents),
// and an in-memory agent registry maintained via heartbeats.
//
// What v0.1 deliberately does NOT ship:
//
//   - gRPC. The same handlers can be exposed over gRPC once the
//     proto definitions land; the in-memory state +
//     dispatcher are already separated to make that a thin layer.
//   - Job dispatch (POST /v1/deployments/<d>/backups, restores,
//     verify). alongside the agent's gRPC streaming progress
//     channel.
//   - OIDC + RBAC. v0.1 is single-token; OIDC + per-verb RBAC land
//     alongside the audit-chain integration.
//   - Persistent state. The control plane treats the repo as the
//     source of truth and keeps in-memory caches only. Persistent
//     dispatch state lands backed by PG (a `pg_hardstorage`
//     schema in any reachable PostgreSQL) or etcd — never a
//     non-PG embedded database.
//
// What ships:
//
//   - HTTP listener on a configurable address with optional TLS +
//     mTLS client-auth.
//   - Bearer-token auth (single token from a config-referenced file)
//     for /v1/* routes; /v1/healthz is unauthenticated.
//   - In-memory AgentRegistry — register, heartbeat, list. Agents
//     missing two heartbeats fall out of "active" listings.
//   - Repo-walking handlers for deployments + backups: the server
//     reads the same manifest format the agent writes, so a
//     read-only fleet view works against an existing agent setup
//     without protocol changes.
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/metrics"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/version"
)

// SchemaResult is the JSON schema string for server response bodies.
// Same 24-month back-compat commitment as the agent's CLI v1
// contract.
const SchemaResult = "pg_hardstorage.server.v1"

// Config configures one server. Loaded from the agent config file's
// top-level `server:` block.
type Config struct {
	// Listen is the bind address. Default: 127.0.0.1:8443. Set to
	// 0.0.0.0:8443 to expose externally.
	Listen string `yaml:"listen,omitempty"`

	// TLS configures the listener's TLS posture. When CertFile +
	// KeyFile are set, the listener serves HTTPS. ClientCAFile, when
	// also set, requires + verifies client certificates (mTLS).
	TLS TLSConfig `yaml:"tls,omitempty"`

	// Auth configures bearer-token authentication. v0.1 reads a
	// single token from TokenFile; OIDC + multi-token RBAC land in
	//.
	Auth AuthConfig `yaml:"auth,omitempty"`

	// Repos are the repositories the server reports on. The
	// `/v1/deployments` endpoint walks each repo; backups
	// endpoints query against the named deployment's repo.
	Repos []string `yaml:"repos,omitempty"`

	// HeartbeatTimeout marks an agent inactive when no heartbeat
	// arrives within this window. Default: 30s. The SPEC's "10s
	// heartbeat, miss two = inactive" gives 30s.
	HeartbeatTimeout time.Duration `yaml:"heartbeat_timeout,omitempty"`

	// MaxConcurrentJobs caps how many dispatched jobs may run at once
	// across the fleet — backpressure so a burst of queued work (or a
	// fleet all polling at once) can't storm storage / PostgreSQL with
	// unbounded concurrent backups. Zero (the default) means unlimited.
	// Applied to the job registry at construction; for multi-control-
	// plane HA set the same value on every control plane (the PG backend
	// enforces it globally via the shared jobs table).
	MaxConcurrentJobs int `yaml:"max_concurrent_jobs,omitempty"`

	// RestoreRoots optionally constrains the absolute target_dir an
	// API client can pass to /v1/deployments/<n>/restores.  When
	// empty (the default) the agent's own filesystem permissions are
	// the only gate.  When non-empty, the request's target_dir must
	// be under one of the listed roots, post-Clean.  Defence-in-
	// depth: avoids a misconfigured client asking the agent to
	// restore into /etc, /usr, …
	RestoreRoots []string `yaml:"restore_roots,omitempty"`
}

// TLSConfig is the TLS / mTLS sub-config.
type TLSConfig struct {
	CertFile     string `yaml:"cert_file,omitempty"`
	KeyFile      string `yaml:"key_file,omitempty"`
	ClientCAFile string `yaml:"client_ca_file,omitempty"`
}

// AuthConfig configures bearer-token auth.
type AuthConfig struct {
	// TokenFile is a path to a file whose first line is the bearer
	// token clients present via Authorization: Bearer <token>. Empty
	// disables bearer-token auth (only safe behind mTLS).
	TokenFile string `yaml:"token_file,omitempty"`
}

// Server is the running control plane.
type Server struct {
	cfg    Config
	logger Logger
	srv    *http.Server
	agents *AgentRegistry
	jobs   *JobRegistry
	token  string
}

// Logger is a tiny structured-log shim. The dispatcher's structured
// Event bus lands here; for now we keep server logs separate
// so the listener can come up before the dispatcher is even
// constructed.
type Logger interface {
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}

// stdLogger is a default Logger that writes to os.Stderr in a stable
// format. Operators wiring the dispatcher's Event bus pass their own
// Logger via WithLogger.
type stdLogger struct{}

// Infof writes an INFO-prefixed line to stderr.
func (stdLogger) Infof(f string, a ...any) { fmt.Fprintf(os.Stderr, "INFO  "+f+"\n", a...) }

// Errorf writes an ERROR-prefixed line to stderr.
func (stdLogger) Errorf(f string, a ...any) { fmt.Fprintf(os.Stderr, "ERROR "+f+"\n", a...) }

// New constructs a Server with the given config. Defaults applied
// here (listen, heartbeat timeout). Errors out only on
// already-detectable misconfiguration; bad TLS / token paths surface
// at Run time.
//
// New uses an in-memory JobBackend. Operators wanting persistence
// (so dispatch state survives a control-plane restart and so multiple
// control planes can coexist) construct via NewWithJobs and pass a
// PG-backed JobRegistry — see internal/server/jobs_pg.go.
func New(cfg Config) (*Server, error) {
	return NewWithJobs(cfg, NewJobRegistry())
}

// NewWithJobs is the explicit-backend constructor. Pass the registry
// you want — typically NewJobRegistryWithBackend(OpenPGBackend(...))
// for production. The CLI's --coord-backend flag flows through
// here.
func NewWithJobs(cfg Config, jobs *JobRegistry) (*Server, error) {
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8443"
	}
	if cfg.HeartbeatTimeout == 0 {
		cfg.HeartbeatTimeout = 30 * time.Second
	}
	if jobs == nil {
		jobs = NewJobRegistry()
	}
	// Apply the fleet-wide job-concurrency cap (0 = unlimited). Set on
	// whatever registry the caller passed (memory default or PG-backed).
	if cfg.MaxConcurrentJobs > 0 {
		jobs.WithMaxConcurrent(cfg.MaxConcurrentJobs)
	}
	s := &Server{
		cfg:    cfg,
		logger: stdLogger{},
		agents: NewAgentRegistry(cfg.HeartbeatTimeout),
		jobs:   jobs,
	}
	if cfg.Auth.TokenFile != "" {
		body, err := os.ReadFile(cfg.Auth.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("server: read token file: %w", err)
		}
		s.token = trimToken(string(body))
		if s.token == "" {
			return nil, errors.New("server: token file is empty")
		}
		if err := assertTokenFileMode(cfg.Auth.TokenFile); err != nil {
			return nil, fmt.Errorf("server: token file: %w", err)
		}
	}
	s.srv = &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.routes(),
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// Publish build metadata so /metrics always carries at least one
	// real sample, even on a control plane that has never dispatched a
	// job.  Idempotent across multiple servers in one process.
	metrics.SetBuildInfo(version.Version, version.Commit)
	return s, nil
}

// WithLogger replaces the default logger.
func (s *Server) WithLogger(l Logger) *Server { s.logger = l; return s }

// Listen returns the actual listen address (after default fill-in).
// Useful for tests that bind to :0 and need to know the assigned port.
func (s *Server) Listen() string { return s.cfg.Listen }

// Handler exposes the server's HTTP mux. Production callers go
// through Run(); tests use Handler() with httptest.NewServer to
// drive the routes without binding a real listener.
func (s *Server) Handler() http.Handler { return s.routes() }

// Run starts the listener and blocks until ctx cancels. On cancel
// we issue a graceful Shutdown; if the shutdown deadline elapses we
// fall back to Close. Returns http.ErrServerClosed wrapped on clean
// stop, or the listener error on bring-up failure.
func (s *Server) Run(ctx context.Context) error {
	tlsCfg, err := s.buildTLSConfig()
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", s.cfg.Listen, err)
	}
	s.cfg.Listen = ln.Addr().String() // pick up the resolved port for :0

	// Start the job-sweeper. Reaps Running jobs whose StartedAt is
	// older than claimDeadline; without this, a crashed agent leaves
	// its job in `running` forever. Runs on a 1-minute tick by
	// default; the deadline itself is configurable via the registry.
	s.jobs.RunSweeper(ctx, time.Minute, func(reaped int, err error) {
		switch {
		case err != nil:
			s.logger.Errorf("jobs: sweeper tick failed: %v", err)
		case reaped > 0:
			s.logger.Infof("jobs: sweeper reaped %d abandoned job(s)", reaped)
		}
	})
	// Stop the sweeper when Run returns. RunSweeper binds the goroutine
	// only to ctx, so if Serve returns on its own (a listener error, or
	// an external srv.Close) WITHOUT a ctx cancel, the sweeper would keep
	// ticking against a possibly-Closed backend. Stop is idempotent and
	// self-sufficient (cancels its own derived context), so it's safe on
	// every exit path.
	defer s.jobs.Stop()

	// Spawn the shutdown goroutine before serving so a ctx cancel
	// during the Serve call routes through Shutdown, not just by
	// closing the listener (which leaves in-flight requests open).
	//
	// It waits on ctx.Done() OR serveDone: if Serve returns on its own
	// (a listener error, or an external srv.Close) WITHOUT a ctx cancel,
	// serveDone releases this goroutine so it closes shutdownDone — Run's
	// `<-shutdownDone` below would otherwise block forever waiting on a
	// goroutine still parked on ctx.Done() (deadlock audit #2). The close
	// is deferred so it fires on every exit path, including a panic.
	shutdownDone := make(chan struct{})
	serveDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = s.srv.Shutdown(shCtx)
		case <-serveDone:
			// Serve already returned without a ctx cancel; nothing left
			// to gracefully shut down.
		}
	}()

	s.logger.Infof("control plane listening on %s (TLS=%v, mTLS=%v, token=%v)",
		s.cfg.Listen, tlsCfg != nil,
		tlsCfg != nil && tlsCfg.ClientAuth >= tls.RequireAndVerifyClientCert,
		s.token != "")

	var serveErr error
	if tlsCfg != nil {
		s.srv.TLSConfig = tlsCfg
		serveErr = s.srv.ServeTLS(ln, "", "")
	} else {
		serveErr = s.srv.Serve(ln)
	}
	// Signal the shutdown goroutine that Serve has returned, so it never
	// stays parked on ctx.Done() when shutdown was triggered by Serve
	// itself rather than a ctx cancel (deadlock audit #2).
	close(serveDone)
	<-shutdownDone
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

// Agents returns the agent registry. Exposed for testing and for
// the dispatcher path that needs to look up agents.
func (s *Server) Agents() *AgentRegistry { return s.agents }

// Jobs returns the job registry. Exposed for testing.
func (s *Server) Jobs() *JobRegistry { return s.jobs }

// Close releases backend resources (e.g. the PG pool when running
// under PGBackend). Safe to call multiple times. Run() does NOT call
// this — the CLI is responsible because the same backend may outlive
// a single Run() invocation in tests.
func (s *Server) Close() error {
	if s.jobs == nil {
		return nil
	}
	if b := s.jobs.Backend(); b != nil {
		return b.Close()
	}
	return nil
}

// buildTLSConfig assembles a *tls.Config from the cfg. Returns nil
// when TLS isn't configured (plain HTTP).
func (s *Server) buildTLSConfig() (*tls.Config, error) {
	if s.cfg.TLS.CertFile == "" && s.cfg.TLS.KeyFile == "" {
		if isLoopback(s.cfg.Listen) {
			return nil, nil
		}
		return nil, errors.New("server: TLS is required for non-loopback listeners; set cert_file + key_file or use a loopback address (127.0.0.1, ::1)")
	}
	if s.cfg.TLS.CertFile == "" || s.cfg.TLS.KeyFile == "" {
		return nil, errors.New("server: TLS requires both cert_file and key_file")
	}
	cert, err := tls.LoadX509KeyPair(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("server: load cert: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}
	if s.cfg.TLS.ClientCAFile != "" {
		caBytes, err := os.ReadFile(s.cfg.TLS.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("server: read client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, errors.New("server: client CA file did not yield any usable certs")
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return tlsCfg, nil
}

// trimToken strips trailing newlines so a token-file written with
// `echo` works without manual editing.
func trimToken(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	return s
}

// assertTokenFileMode ensures the token file has mode 0600 (owner
// read-write only). A world-readable token file leaks the bearer
// credential to every local user — the token is at least as
// sensitive as the KEK.
func assertTokenFileMode(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if m := fi.Mode().Perm(); m != 0o600 {
		return fmt.Errorf("token file %q has mode %04o; expected 0600 — no group/world access", path, m)
	}
	return nil
}

// isLoopback returns true when addr is a loopback address.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func init() { _ = net.Listen }

// --- handler shared helpers ------------------------------------------

// envelope wraps a handler's response in the same shape the CLI
// emits via the dispatcher. Stable across versions per the
// pg_hardstorage.server.v1 contract.
type envelope struct {
	Schema      string   `json:"schema"`
	Command     string   `json:"command,omitempty"`
	GeneratedAt string   `json:"generated_at"`
	Result      any      `json:"result,omitempty"`
	Error       *errBody `json:"error,omitempty"`
}

type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSON serialises e and writes it. status is the HTTP status.
// Errors hit the server logger; we don't surface them to the client
// past what the status code already indicates.
func (s *Server) writeJSON(w http.ResponseWriter, status int, e envelope) {
	if e.Schema == "" {
		e.Schema = SchemaResult
	}
	if e.GeneratedAt == "" {
		e.GeneratedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	body, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		s.logger.Errorf("encode envelope: %v", err)
		return
	}
	body = append(body, '\n')
	_, _ = w.Write(body)
}

// writeError is the structured-error helper. Used by every handler
// for non-2xx responses.
//
// http.ResponseWriter is single-goroutine by contract (the framework
// hands one Writer per request to one handler call), so writeError
// itself takes no lock — the earlier dead `sync.Mutex{}` here was a
// local-per-call mutex that synchronised nothing.
func (s *Server) writeError(w http.ResponseWriter, status int, code, message string) {
	s.writeJSON(w, status, envelope{
		Error: &errBody{Code: code, Message: message},
	})
}

// versionInfo is the body returned by GET /v1/version.
type versionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

func currentVersion() versionInfo {
	return versionInfo{
		Version: version.Version,
		Commit:  version.Commit,
		Date:    version.Date,
	}
}
