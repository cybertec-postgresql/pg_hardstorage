// routes.go — REST handlers (jobs/deployments/backups/restores/verifies) with rate + result caps.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// API rate-limit + result-cap constants.  The control-plane API
// must not let an unauthenticated GET return an unbounded result
// set (memory DoS).  These caps apply uniformly to every list
// endpoint that takes a `?limit=` query param.
const (
	// DefaultJobsLimit is what `?limit=` defaults to when absent.
	DefaultJobsLimit = 200
	// MaxJobsLimit is the hard ceiling.  Values above clamp here.
	MaxJobsLimit = 10000

	// MaxJSONRequestBytes is the cap every POST endpoint applies
	// to its request body before invoking json.Decode.  Audit v25
	// #2: without this, a malicious client could POST an
	// unbounded body and the server would allocate while parsing.
	// 1 MiB is comfortable for the largest legitimate payload
	// (an audit-event POST with a structured body); anything
	// bigger should be rejected as "too large" rather than
	// silently parsed.
	MaxJSONRequestBytes = 1 << 20
)

// decodeJSONBody decodes r.Body into dst with a hard size cap +
// strict-unknown-fields refusal.  Returns a structured error the
// caller routes to writeError; the cap-exceeded case maps to a
// 413 Payload-Too-Large via the *http.MaxBytesError type guard.
//
// An audit fix.  Centralised so every POST handler gets the
// cap automatically and the unknown-field policy stays uniform.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxJSONRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// parseLimit clamps a user-supplied `?limit=` to [1, max].
// Returns an error for malformed input (so the caller emits 400
// rather than silently fudging the value).
//
// Defensive: prior versions used `fmt.Sscanf(v, "%d", &opts.Limit)`
// and didn't reject negative values.  Combined with `if Limit > 0`
// gates further down the stack, `?limit=-1` produced an unbounded
// query.  This helper is the chokepoint for that whole class of
// DoS.
func parseLimit(raw string, def, max int) (int, error) {
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("limit must be a non-negative integer (got %q)", raw)
	}
	if n < 0 {
		return 0, fmt.Errorf("limit must be ≥ 0 (got %d)", n)
	}
	if n == 0 {
		return def, nil
	}
	if n > max {
		return max, nil
	}
	return n, nil
}

// validateRestoreTargetDir performs defence-in-depth checks on a
// caller-supplied restore target directory:
//
//   - non-empty
//   - absolute (prevents working-directory ambiguity on the agent)
//   - normalised (no `.` / `..` traversal segments after Clean)
//   - within the configured root, when one is set on the server
//
// Path validation here doesn't replace OS-level permission checks
// — the agent's user must still have write rights on the target
// — but it stops a misconfigured caller from asking the agent to
// drop files in an unintended root.
func validateRestoreTargetDir(target string, allowedRoots []string) error {
	if target == "" {
		return errors.New("target_dir is required")
	}
	if !filepath.IsAbs(target) {
		return fmt.Errorf("target_dir must be absolute (got %q)", target)
	}
	clean := filepath.Clean(target)
	if clean != target {
		// Reject inputs that contain ".." / "." / duplicate slashes
		// — the cleaned form differs from the original.  We could
		// silently accept the cleaned form but prefer the operator
		// see the rejection so the misconfiguration is obvious.
		return fmt.Errorf("target_dir is not normalised; got %q, want %q", target, clean)
	}
	if len(allowedRoots) == 0 {
		return nil
	}
	for _, root := range allowedRoots {
		root = filepath.Clean(root)
		rel, err := filepath.Rel(root, clean)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("target_dir %q is outside the allowed roots", target)
}

// validateRestorePITRArgs checks the PG-typed PITR fields on a restore
// enqueue body so the operator gets a precise local error instead of a
// queued job that fails as soon as the agent tries to translate the
// recovery_target_* GUCs. Mirrors the CLI-side check.
// Returns (code, message, ok=true) on success; (code, message, ok=false)
// names which field tripped.
func validateRestorePITRArgs(args map[string]any) (code, message string, ok bool) {
	if v, isStr := args["to_lsn"].(string); isStr && v != "" {
		if !restore.LooksLikeLSN(v) {
			return "usage.bad_lsn",
				fmt.Sprintf("to_lsn %q: expected PG LSN hex form like 0/3000028", v), false
		}
	}
	if v, isStr := args["to_action"].(string); isStr {
		switch v {
		case "", "pause", "promote", "shutdown":
		default:
			return "usage.bad_action",
				fmt.Sprintf("to_action %q: must be one of pause|promote|shutdown", v), false
		}
	}
	if v, isStr := args["to_timeline"].(string); isStr && v != "" && v != "latest" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil || n == 0 {
			return "usage.bad_timeline",
				fmt.Sprintf("to_timeline %q: must be \"latest\" or a positive integer", v), false
		}
	}
	return "", "", true
}

// routes wires the HTTP handlers. The mux is small enough to spell
// out by hand; introducing a router framework for ~10 routes would
// be overkill.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/readyz", s.handleReadyz)
	mux.HandleFunc("/v1/version", s.requireAuth(s.handleVersion))
	mux.HandleFunc("/v1/deployments", s.requireAuth(s.handleDeployments))
	mux.HandleFunc("/v1/deployments/", s.requireAuth(s.handleDeploymentsSubtree))
	mux.HandleFunc("/v1/agents", s.requireAuth(s.handleAgents))
	mux.HandleFunc("/v1/agents/heartbeat", s.requireAuth(s.handleAgentsHeartbeat))
	mux.HandleFunc("/v1/jobs", s.requireAuth(s.handleJobs))
	mux.HandleFunc("/v1/jobs/", s.requireAuth(s.handleJobsSubtree))
	mux.HandleFunc("/v1/jobs/claim", s.requireAuth(s.handleJobsClaim))
	// /metrics is unauthenticated (like the health probes) so a
	// Prometheus scraper needs no operator credential; see handleMetrics
	// for the rationale.  Every route — metrics included — is wrapped so
	// requests are counted into pg_hardstorage_http_requests_total.
	mux.HandleFunc("/metrics", s.handleMetrics)
	return withHTTPMetrics(mux)
}

// requireAuth wraps a handler with bearer-token verification. Skips
// auth when the server has no token configured (intended for
// behind-mTLS deployments where the client cert IS the auth).
func (s *Server) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			h(w, r)
			return
		}
		got := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(got, prefix) || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(got, prefix)), []byte(s.token)) != 1 {
			s.writeError(w, http.StatusUnauthorized, "auth.invalid_token",
				"missing or invalid bearer token")
			return
		}
		h(w, r)
	}
}

// --- handlers --------------------------------------------------------

// handleHealthz is the liveness probe. No auth, no state — returns
// 200 if the listener is serving.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, envelope{
		Result: map[string]string{"status": "ok"},
	})
}

// handleReadyz is the readiness probe. Returns 200 only when every
// configured repo is reachable. A repo's read failure trips the
// probe; agent registrations are NOT required to be ready (the
// control plane works without agents — operators can still query).
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	type repoCheck struct {
		URL   string `json:"url"`
		Ready bool   `json:"ready"`
		Error string `json:"error,omitempty"`
	}
	out := make([]repoCheck, 0, len(s.cfg.Repos))
	allReady := true
	for _, repoURL := range s.cfg.Repos {
		// readyz is UNAUTHENTICATED (k8s / load-balancer probe), so it
		// must not echo secrets. Repo URLs can embed userinfo
		// (sftp://user:pass@host) and query-string credentials (azure
		// ?sig=, s3 ?X-Amz-Signature=), and repo.Open's error wraps the
		// URL + backend connection detail. Redact both before they reach
		// an anonymous caller.
		check := repoCheck{URL: redactRepoURL(repoURL), Ready: true}
		// Per-iteration closure so defer fires on every path
		// — including a panic between Open and Close. Without
		// the closure, `defer sp.Close()` would queue at
		// function scope and accumulate one closure per repo
		// before any fired.
		func() {
			_, sp, err := repo.Open(r.Context(), repoURL)
			if err != nil {
				check.Ready = false
				check.Error = redactRepoErr(err, repoURL)
				allReady = false
				return
			}
			defer sp.Close()
		}()
		out = append(out, check)
	}
	status := http.StatusOK
	if !allReady {
		status = http.StatusServiceUnavailable
	}
	s.writeJSON(w, status, envelope{
		Result: map[string]any{
			"status": map[bool]string{true: "ready", false: "degraded"}[allReady],
			"repos":  out,
		},
	})
}

// redactRepoURL strips secrets from a repo URL so the unauthenticated
// readiness probe can name a repo without leaking credentials: it masks
// any userinfo password (sftp://user:pass@host → sftp://user:xxxxx@host)
// via url.Redacted() and drops the query + fragment entirely, since repo
// URLs carry SAS tokens / presigned signatures there (azure ?sig=...,
// s3 ?X-Amz-Signature=...). A URL that won't parse is replaced wholesale
// rather than risk echoing an embedded secret. Mirrors the repo-URL
// redaction the CLI already applies (internal/cli/timetable.go).
func redactRepoURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable repo url>"
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.Redacted()
}

// redactRepoErr scrubs a repo.Open error for the unauthenticated readyz
// response. The backend error wraps the raw URL (and may surface the bare
// password), so replace every occurrence of the raw URL with its redacted
// form and mask the password verbatim. The reason text (connection
// refused, auth failed, timeout, ...) survives for diagnosis.
func redactRepoErr(err error, repoURL string) string {
	msg := err.Error()
	msg = strings.ReplaceAll(msg, repoURL, redactRepoURL(repoURL))
	if u, perr := url.Parse(repoURL); perr == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			msg = strings.ReplaceAll(msg, pw, "xxxxx")
		}
	}
	return msg
}

// handleVersion returns build info. Authenticated — version info is
// useful for an authorised client to verify the rolling-upgrade
// state of the control plane fleet.
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, envelope{
		Result: currentVersion(),
	})
}

// handleDeployments lists every deployment across every configured
// repo. The result merges across repos: a deployment present in two
// repos shows up once with the repo URL recorded.
//
// v0.1 query semantics: walk each repo's manifest store, collect the
// set of unique deployment names. Backup-count and last-backup are
// loaded eagerly so a single GET answers the operator's most common
// question ("what's the state of my fleet?") without N follow-up
// requests.
func (s *Server) handleDeployments(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		Name        string `json:"name"`
		Repo        string `json:"repo"`
		BackupCount int    `json:"backup_count"`
		LastBackup  string `json:"last_backup,omitempty"`
		LastAt      string `json:"last_at,omitempty"`
	}
	out := make([]entry, 0)
	for _, url := range s.cfg.Repos {
		// Per-iteration closure so defer fires on every
		// return path — open errors, list errors, and the
		// happy path all release the storage handle without
		// a per-branch sp.Close(). Panic-safe.
		func() {
			_, sp, err := repo.Open(r.Context(), url)
			if err != nil {
				s.logger.Errorf("deployments: open %s: %v", url, err)
				return
			}
			defer sp.Close()
			ms := backup.NewManifestStore(sp)
			deployments, err := ms.Deployments(r.Context())
			if err != nil {
				s.logger.Errorf("deployments: list %s: %v", url, err)
				return
			}
			for _, dep := range deployments {
				e := entry{Name: dep, Repo: url}
				// ListAttestationless: this endpoint just reports
				// counts + last-backup pointers; trust is enforced
				// at the API auth layer above.  ms.List with nil
				// verifier would reject every signed manifest and
				// underreport every deployment as empty.
				var lastAt time.Time
				for m, lerr := range ms.ListAttestationless(r.Context(), dep) {
					if lerr != nil || m == nil {
						continue
					}
					e.BackupCount++
					// "last backup" = newest by StoppedAt, not the
					// lexicographic max of BackupID: the ID encodes
					// type ("full"/"incremental_lsn"/"snapshot") before
					// the timestamp, so max(BackupID) would report an
					// older incremental as the last backup (with its
					// StoppedAt). Tie-break by BackupID for determinism.
					if e.LastBackup == "" || m.StoppedAt.After(lastAt) ||
						(m.StoppedAt.Equal(lastAt) && m.BackupID > e.LastBackup) {
						e.LastBackup = m.BackupID
						e.LastAt = m.StoppedAt.Format("2006-01-02T15:04:05Z")
						lastAt = m.StoppedAt
					}
				}
				out = append(out, e)
			}
		}()
	}
	s.writeJSON(w, http.StatusOK, envelope{
		Result: map[string]any{"deployments": out},
	})
}

// handleDeploymentsSubtree dispatches /v1/deployments/<name>/<verb>
// where verb is `backups` (GET = list, POST = enqueue).
func (s *Server) handleDeploymentsSubtree(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/v1/deployments/")
	parts := strings.SplitN(tail, "/", 3)
	if len(parts) < 2 {
		s.writeError(w, http.StatusNotFound, "notfound.route",
			"want /v1/deployments/<name>/<verb>")
		return
	}
	name, verb := parts[0], parts[1]
	switch verb {
	case "backups":
		switch r.Method {
		case http.MethodGet:
			s.handleDeploymentBackups(w, r, name)
		case http.MethodPost:
			s.handleEnqueueBackup(w, r, name)
		default:
			s.writeError(w, http.StatusMethodNotAllowed, "usage.method",
				"deployments/<n>/backups: only GET or POST is supported")
		}
	case "restores":
		switch r.Method {
		case http.MethodPost:
			s.handleEnqueueRestore(w, r, name)
		default:
			s.writeError(w, http.StatusMethodNotAllowed, "usage.method",
				"deployments/<n>/restores: only POST is supported")
		}
	case "verifies":
		switch r.Method {
		case http.MethodPost:
			s.handleEnqueueVerify(w, r, name)
		default:
			s.writeError(w, http.StatusMethodNotAllowed, "usage.method",
				"deployments/<n>/verifies: only POST is supported")
		}
	default:
		s.writeError(w, http.StatusNotFound, "notfound.route",
			"unknown verb "+verb+"; supports `backups`, `restores`, `verifies`")
	}
}

// handleDeploymentBackups returns the named deployment's committed
// manifests across every configured repo. Newest first.
func (s *Server) handleDeploymentBackups(w http.ResponseWriter, r *http.Request, name string) {
	type entry struct {
		BackupID  string `json:"backup_id"`
		Repo      string `json:"repo"`
		Type      string `json:"type"`
		PGVersion int    `json:"pg_version"`
		Timeline  uint32 `json:"timeline"`
		StartLSN  string `json:"start_lsn"`
		StopLSN   string `json:"stop_lsn"`
		StartedAt string `json:"started_at"`
		StoppedAt string `json:"stopped_at"`
	}
	out := make([]entry, 0)
	for _, url := range s.cfg.Repos {
		// Per-iteration closure so defer fires on every path.
		func() {
			_, sp, err := repo.Open(r.Context(), url)
			if err != nil {
				return
			}
			defer sp.Close()
			ms := backup.NewManifestStore(sp)
			// ListAttestationless: see the explanation on the
			// sibling endpoint above — listing for the API isn't a
			// trust path, and ms.List with nil rejects every
			// signed manifest.
			for m, lerr := range ms.ListAttestationless(r.Context(), name) {
				if lerr != nil || m == nil {
					continue
				}
				out = append(out, entry{
					BackupID:  m.BackupID,
					Repo:      url,
					Type:      string(m.Type),
					PGVersion: m.PGVersion,
					Timeline:  m.Timeline,
					StartLSN:  m.StartLSN,
					StopLSN:   m.StopLSN,
					StartedAt: m.StartedAt.Format("2006-01-02T15:04:05Z"),
					StoppedAt: m.StoppedAt.Format("2006-01-02T15:04:05Z"),
				})
			}
		}()
	}
	// Newest first — backup IDs sort lex == chronological.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	s.writeJSON(w, http.StatusOK, envelope{
		Result: map[string]any{"deployment": name, "backups": out},
	})
}

// handleAgents lists registered agents. ?include_inactive=true
// surfaces agents past the heartbeat timeout.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "usage.method",
			"agents: only GET is supported on this endpoint")
		return
	}
	includeInactive := r.URL.Query().Get("include_inactive") == "true"
	out := s.agents.List(includeInactive)
	s.writeJSON(w, http.StatusOK, envelope{
		Result: map[string]any{
			"agents":            out,
			"heartbeat_timeout": s.cfg.HeartbeatTimeout.String(),
		},
	})
}

// handleAgentsHeartbeat is the agent-side endpoint. Agents POST a
// HeartbeatRequest body every ~10s.
func (s *Server) handleAgentsHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "usage.method",
			"agents/heartbeat: only POST is supported")
		return
	}
	var req HeartbeatRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "usage.bad_body",
			"agents/heartbeat: parse body: "+err.Error())
		return
	}
	a, err := s.agents.Heartbeat(req)
	if err != nil {
		var bad errBadHeartbeat
		if errors.As(err, &bad) {
			s.writeError(w, http.StatusBadRequest, "usage.bad_heartbeat", err.Error())
			return
		}
		s.writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, envelope{Result: a})
}

// --- job dispatch ----------------------------------------------------

// handleEnqueueBackup is POST /v1/deployments/<n>/backups. Body is
// optional; when provided, fields ride into Job.Args so the agent
// can honour --type=full|incremental, --tag, etc.
func (s *Server) handleEnqueueBackup(w http.ResponseWriter, r *http.Request, deployment string) {
	var args map[string]any
	if r.ContentLength > 0 {
		if err := decodeJSONBody(w, r, &args); err != nil {
			s.writeError(w, http.StatusBadRequest, "usage.bad_body",
				"deployments/<n>/backups: parse body: "+err.Error())
			return
		}
	}
	repoURL := ""
	if v, ok := args["repo"].(string); ok {
		repoURL = v
	}
	if repoURL == "" && len(s.cfg.Repos) > 0 {
		repoURL = s.cfg.Repos[0]
	}
	job, err := s.jobs.Enqueue(EnqueueOptions{
		Kind:       JobBackup,
		Deployment: deployment,
		RepoURL:    repoURL,
		Args:       args,
	})
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "usage.bad_enqueue", err.Error())
		return
	}
	s.writeJSON(w, http.StatusAccepted, envelope{Result: job})
}

// handleEnqueueVerify is POST /v1/deployments/<n>/verifies. Body is
// JSON with at minimum a backup_id (or "latest"); pg_major + tempdir
// are optional. The agent's VerifyExecutor performs the full restore-
// to-sandbox + pg_verifybackup loop and returns a structured result.
//
// Body shape:
//
//	{
//	  "backup_id": "db1.full.…|latest",
//	  "pg_major":  "17",          // optional override
//	  "tempdir":   "/var/lib/pg_hardstorage/verify-tmp"   // optional
//	}
func (s *Server) handleEnqueueVerify(w http.ResponseWriter, r *http.Request, deployment string) {
	var args map[string]any
	if r.ContentLength <= 0 {
		s.writeError(w, http.StatusBadRequest, "usage.bad_body",
			"deployments/<n>/verifies: body required (JSON with backup_id)")
		return
	}
	if err := decodeJSONBody(w, r, &args); err != nil {
		s.writeError(w, http.StatusBadRequest, "usage.bad_body",
			"deployments/<n>/verifies: parse body: "+err.Error())
		return
	}
	backupID, _ := args["backup_id"].(string)
	if backupID == "" {
		s.writeError(w, http.StatusBadRequest, "usage.missing_field",
			"deployments/<n>/verifies: backup_id is required (or \"latest\")")
		return
	}
	repoURL := ""
	if v, ok := args["repo"].(string); ok {
		repoURL = v
	}
	if repoURL == "" && len(s.cfg.Repos) > 0 {
		repoURL = s.cfg.Repos[0]
	}
	job, err := s.jobs.Enqueue(EnqueueOptions{
		Kind:       JobVerify,
		Deployment: deployment,
		RepoURL:    repoURL,
		Args:       args,
	})
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "usage.bad_enqueue", err.Error())
		return
	}
	s.writeJSON(w, http.StatusAccepted, envelope{Result: job})
}

// handleEnqueueRestore is POST /v1/deployments/<n>/restores. Body is
// JSON; required fields surface as structured errors so a misbehaving
// CLI sees a parseable code rather than 500. Body shape:
//
//	{
//	  "backup_id":  "db1.full.20260427T0900Z",   // or "latest"
//	  "target_dir": "/var/lib/postgresql/restored",
//	  "repo":       "s3://acme-pg-backups/",     // optional, defaults to server.repos[0]
//	  "allow_overwrite": false,                  // refuse non-empty target unless true
//	  "to":         "5 minutes ago",             // optional PITR target (natural-language)
//	  "to_lsn":     "0/3000028",                 // optional PITR target (LSN)
//	  "verify_after": true                       // optional pg_verifybackup gate
//	}
//
// Required: backup_id, target_dir. Everything else flows into
// Job.Args; the agent's RestoreExecutor parses them.
func (s *Server) handleEnqueueRestore(w http.ResponseWriter, r *http.Request, deployment string) {
	var args map[string]any
	if r.ContentLength <= 0 {
		s.writeError(w, http.StatusBadRequest, "usage.bad_body",
			"deployments/<n>/restores: body required (JSON with backup_id and target_dir)")
		return
	}
	if err := decodeJSONBody(w, r, &args); err != nil {
		s.writeError(w, http.StatusBadRequest, "usage.bad_body",
			"deployments/<n>/restores: parse body: "+err.Error())
		return
	}
	// Required fields. We validate at the route boundary so the
	// agent doesn't have to refuse a well-formed claim with a
	// malformed payload — the operator gets the error at the source.
	backupID, _ := args["backup_id"].(string)
	if backupID == "" {
		s.writeError(w, http.StatusBadRequest, "usage.missing_field",
			"deployments/<n>/restores: backup_id is required (or \"latest\")")
		return
	}
	targetDir, _ := args["target_dir"].(string)
	if err := validateRestoreTargetDir(targetDir, s.cfg.RestoreRoots); err != nil {
		s.writeError(w, http.StatusBadRequest, "usage.bad_target_dir",
			"deployments/<n>/restores: "+err.Error())
		return
	}
	if code, msg, ok := validateRestorePITRArgs(args); !ok {
		s.writeError(w, http.StatusBadRequest, code,
			"deployments/<n>/restores: "+msg)
		return
	}
	repoURL := ""
	if v, ok := args["repo"].(string); ok {
		repoURL = v
	}
	if repoURL == "" && len(s.cfg.Repos) > 0 {
		repoURL = s.cfg.Repos[0]
	}
	job, err := s.jobs.Enqueue(EnqueueOptions{
		Kind:       JobRestore,
		Deployment: deployment,
		RepoURL:    repoURL,
		Args:       args,
	})
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "usage.bad_enqueue", err.Error())
		return
	}
	s.writeJSON(w, http.StatusAccepted, envelope{Result: job})
}

// handleJobs is GET /v1/jobs?state=&kind=&deployment=&limit=.
//
// `limit` is clamped to [1, MaxJobsLimit].  Negative or malformed
// values are rejected with HTTP 400.  The default (when `limit` is
// absent) is DefaultJobsLimit so a GET cannot retrieve an unbounded
// result set.  This is a deliberate DoS mitigation: prior versions
// accepted `?limit=-1` and walked every job in the registry, which
// on a busy control plane could exhaust memory.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "usage.method",
			"jobs: only GET is supported on this endpoint")
		return
	}
	q := r.URL.Query()
	opts := ListOptions{
		State:      JobState(q.Get("state")),
		Kind:       JobKind(q.Get("kind")),
		Deployment: q.Get("deployment"),
	}
	limit, err := parseLimit(q.Get("limit"), DefaultJobsLimit, MaxJobsLimit)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "usage.bad_limit", err.Error())
		return
	}
	opts.Limit = limit
	out := s.jobs.List(opts)
	s.writeJSON(w, http.StatusOK, envelope{
		Result: map[string]any{"jobs": out, "count": len(out)},
	})
}

// handleJobsClaim is POST /v1/jobs/claim. Body declares the calling
// agent + which deployments + kinds the agent will run.
//
// Returns 200 + Job on success, 404 with code notfound.no_jobs when
// no eligible job is available (so polling agents can distinguish
// "no work" from "real error" via the structured code).
func (s *Server) handleJobsClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "usage.method",
			"jobs/claim: only POST is supported")
		return
	}
	var req struct {
		AgentID     string    `json:"agent_id"`
		Deployments []string  `json:"deployments,omitempty"`
		Kinds       []JobKind `json:"kinds,omitempty"`
	}
	if err := decodeJSONBody(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "usage.bad_body",
			"jobs/claim: parse body: "+err.Error())
		return
	}
	job, err := s.jobs.Claim(ClaimOptions{
		AgentID:     req.AgentID,
		Deployments: req.Deployments,
		Kinds:       req.Kinds,
	})
	if err != nil {
		if errors.Is(err, ErrNoJobs) {
			s.writeError(w, http.StatusNotFound, "notfound.no_jobs",
				"no eligible jobs for this agent")
			return
		}
		s.writeError(w, http.StatusBadRequest, "usage.bad_claim", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, envelope{Result: job})
}

// handleJobsSubtree dispatches /v1/jobs/<id>/<verb>:
//
//	GET  /v1/jobs/<id>             — job detail
//	POST /v1/jobs/<id>/progress    — append progress event
//	POST /v1/jobs/<id>/complete    — terminal state transition
//	POST /v1/jobs/<id>/cancel      — operator-initiated cancel
func (s *Server) handleJobsSubtree(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
	if tail == "claim" {
		s.handleJobsClaim(w, r)
		return
	}
	parts := strings.SplitN(tail, "/", 2)
	id := parts[0]
	if id == "" {
		s.writeError(w, http.StatusNotFound, "notfound.route",
			"want /v1/jobs/<id>[/<verb>]")
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			s.writeError(w, http.StatusMethodNotAllowed, "usage.method",
				"jobs/<id>: only GET is supported on this endpoint")
			return
		}
		s.handleJobGet(w, r, id)
		return
	}
	verb := parts[1]
	switch verb {
	case "progress":
		s.handleJobProgress(w, r, id)
	case "complete":
		s.handleJobComplete(w, r, id)
	case "cancel":
		s.handleJobCancel(w, r, id)
	default:
		s.writeError(w, http.StatusNotFound, "notfound.route",
			"unknown verb "+verb+"; v0.1 supports progress|complete|cancel")
	}
}

func (s *Server) handleJobGet(w http.ResponseWriter, _ *http.Request, id string) {
	j, err := s.jobs.Get(id)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "notfound.job",
			"jobs/"+id+": "+err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, envelope{Result: j})
}

func (s *Server) handleJobProgress(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "usage.method",
			"jobs/<id>/progress: only POST is supported")
		return
	}
	var ev ProgressEvent
	if err := decodeJSONBody(w, r, &ev); err != nil {
		s.writeError(w, http.StatusBadRequest, "usage.bad_body",
			"jobs/<id>/progress: parse body: "+err.Error())
		return
	}
	if err := s.jobs.AppendProgress(id, ev); err != nil {
		if errors.Is(err, ErrJobNotFound) {
			s.writeError(w, http.StatusNotFound, "notfound.job", err.Error())
			return
		}
		if errors.Is(err, ErrJobNotRunning) {
			s.writeError(w, http.StatusConflict, "conflict.job_state", err.Error())
			return
		}
		s.writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.writeJSON(w, http.StatusAccepted, envelope{
		Result: map[string]any{"appended": true, "id": id},
	})
}

func (s *Server) handleJobComplete(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "usage.method",
			"jobs/<id>/complete: only POST is supported")
		return
	}
	var req struct {
		Success bool           `json:"success"`
		Result  map[string]any `json:"result,omitempty"`
		Failure string         `json:"failure,omitempty"`
	}
	if err := decodeJSONBody(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "usage.bad_body",
			"jobs/<id>/complete: parse body: "+err.Error())
		return
	}
	j, err := s.jobs.Complete(id, CompleteOptions{
		Success: req.Success,
		Result:  req.Result,
		Failure: req.Failure,
	})
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			s.writeError(w, http.StatusNotFound, "notfound.job", err.Error())
			return
		}
		// Claim was reclaimed (swept abandoned) or cancelled while the
		// agent ran — a 409 tells the agent its result was not recorded,
		// distinct from a transient 5xx it should retry (race-condition
		// audit #3).
		if errors.Is(err, ErrClaimLost) {
			s.writeError(w, http.StatusConflict, "conflict.claim_lost", err.Error())
			return
		}
		s.writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, envelope{Result: j})
}

func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "usage.method",
			"jobs/<id>/cancel: only POST is supported")
		return
	}
	var req struct {
		Reason string `json:"reason,omitempty"`
	}
	// Body is optional on cancel — operators can cancel without
	// specifying a reason.  an internal audit/#4 noted the previous
	// `_ = json.NewDecoder(r.Body).Decode(&req)` swallowed both
	// "no body" (legitimate) and "10 GiB body" (DoS) under the
	// same path.  decodeJSONBody applies the size cap; we still
	// tolerate an empty body (io.EOF) since reason is optional.
	if err := decodeJSONBody(w, r, &req); err != nil && !errors.Is(err, io.EOF) {
		s.writeError(w, http.StatusBadRequest, "usage.bad_body",
			"jobs/<id>/cancel: parse body: "+err.Error())
		return
	}
	reason := req.Reason
	if reason == "" {
		reason = "operator-initiated"
	}
	j, err := s.jobs.Cancel(id, reason)
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			s.writeError(w, http.StatusNotFound, "notfound.job", err.Error())
			return
		}
		s.writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, envelope{Result: j})
}

// silence unused-import on niche build paths.
var _ = context.Background
