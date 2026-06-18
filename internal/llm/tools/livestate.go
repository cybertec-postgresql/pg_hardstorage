// livestate.go — live-state LLM tools that shell out to `pg_hardstorage` via CLIRunner.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/docs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// LiveStateTools wires every "read live state" tool against a
// CLIRunner.  Production callers get a runner via ResolveSelf;
// tests substitute a stub runner that returns canned JSON.
//
// We deliberately don't pre-register these in init() the way the
// safe-everywhere tools (suggest_command, preview_command,
// read_runbook) are — live-state tools need a CLIRunner, which
// is a runtime construct.  Callers wire them at chat-session
// start via RegisterCoreTools.
type LiveStateTools struct {
	Runner *CLIRunner
}

// RegisterCoreTools registers every live-state tool against the
// given Registry, bound to runner.  Tools registered here:
//
//	read_doctor          — `pg_hardstorage doctor [-d <deployment>]`
//	read_status          — `pg_hardstorage status [<deployment>]`
//	list_deployments     — `pg_hardstorage deployment list`
//	list_backups         — `pg_hardstorage list <deployment>`
//	read_backup          — `pg_hardstorage show <deployment> <backup_id>`
//	read_repo_usage      — `pg_hardstorage repo usage <repo_url>`
//	read_audit           — `pg_hardstorage audit search <filters>`
//	list_runbooks        — bundled doc corpus index
//	search_docs          — bundled doc corpus full-text search
//
// The two doc tools don't need a CLIRunner — they hit the
// embedded corpus; the rest do.
func RegisterCoreTools(reg *Registry, runner *CLIRunner) {
	if reg == nil {
		panic("tools: RegisterCoreTools requires a non-nil Registry")
	}
	if runner == nil {
		panic("tools: RegisterCoreTools requires a non-nil CLIRunner")
	}
	reg.Register(&readDoctor{runner: runner})
	reg.Register(&readStatus{runner: runner})
	reg.Register(&listDeployments{runner: runner})
	reg.Register(&listBackups{runner: runner})
	reg.Register(&readBackup{runner: runner})
	reg.Register(&readRepoUsage{runner: runner})
	reg.Register(&readAudit{runner: runner})
	reg.Register(&listRunbooks{})
	reg.Register(&searchDocs{})
}

// repoResolver is the package-level hook for resolving a
// repository URL from "ambient context" (the operator's config).
// Production wiring uses resolveRepoFromConfig; tests substitute
// a stub that returns "" so the tools shell out the same way the
// pre-fix code did (positional + flags only).  This indirection
// avoids loading the operator's config inside livestate_test.go.
var repoResolver = resolveRepoFromConfig

// resolveRepoFromConfig tries to read the operator's config and
// returns a repository URL.  Resolution order:
//
//  1. <deployment>'s configured repo, if deployment is non-empty
//     and present in the config.
//  2. Top-level `repo:` field (shared default).
//  3. Any deployment's repo, deterministically (first by name).
//  4. Empty string when no config / no repo at all.
//
// Returns ("", nil) on missing config — callers fall back gracefully.
// Returns a non-empty URL when one is resolvable.  Surfaced as a
// behavioural fix for the tool-wrapper bug pilot run 20260514T085557Z
// case C1 caught (read_status was calling `pg_hardstorage status`
// without --repo, which the CLI requires; the LLM session then
// got an empty answer because every cluster-state tool failed).
func resolveRepoFromConfig(deployment string) string {
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return ""
	}
	res, err := config.Load(p)
	if err != nil || res == nil {
		return ""
	}
	if deployment != "" {
		if dep, ok := res.Config.Deployments[deployment]; ok && dep.Repo != "" {
			return dep.Repo
		}
	}
	// Fallback: any deployment's repo (sorted by name for
	// determinism — Go map iteration is randomised).
	var names []string
	for n := range res.Config.Deployments {
		names = append(names, n)
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	for _, n := range names {
		if res.Config.Deployments[n].Repo != "" {
			return res.Config.Deployments[n].Repo
		}
	}
	return ""
}

// ----- read_doctor -----

type readDoctor struct{ runner *CLIRunner }

// Name returns the tool identifier "read_doctor".
func (readDoctor) Name() string { return "read_doctor" }

// Description returns the model-facing summary of the tool's purpose.
func (readDoctor) Description() string {
	return "Run `pg_hardstorage doctor` and return its structured report. Optionally scoped to a single deployment."
}

// Schema returns the JSON-schema-shaped argument descriptor.
func (readDoctor) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"deployment": map[string]any{
				"type":        "string",
				"description": "Optional deployment name to scope the check; empty runs every deployment.",
			},
		},
	}
}

// ReadOnly reports that read_doctor does not mutate state.
func (readDoctor) ReadOnly() bool { return true }

// Run shells out to `pg_hardstorage doctor` and returns the structured
// report. Non-zero exit with a body is treated as "issues found", not a
// tool failure.
func (t *readDoctor) Run(ctx context.Context, args map[string]any) (Result, error) {
	cmd := []string{"doctor"}
	// `pg_hardstorage doctor` takes <deployment> as a POSITIONAL
	// argument, not as `-d <deployment>` — the latter was
	// rejected with "unknown shorthand flag: 'd' in -d" on the
	// pilot's case C1.
	if dep, _ := args["deployment"].(string); dep != "" {
		cmd = append(cmd, dep)
	}
	body, err := t.runner.RunJSON(ctx, cmd...)
	if err != nil {
		// doctor exits non-zero when issues are present (exit 10);
		// that's not a tool failure — surface the body anyway.
		if errors.Is(err, ErrNonZeroExit) && len(body) > 0 {
			return parseAsResult("doctor reported issues", body)
		}
		return Result{}, err
	}
	return parseAsResult("doctor: all clear", body)
}

// ----- read_status -----

type readStatus struct{ runner *CLIRunner }

// Name returns the tool identifier "read_status".
func (readStatus) Name() string { return "read_status" }

// Description returns the model-facing summary of the tool's purpose.
func (readStatus) Description() string {
	return "Run `pg_hardstorage status` and return the structured fleet status (RPO, last-backup, WAL lag, health flags)."
}

// Schema returns the JSON-schema-shaped argument descriptor.
func (readStatus) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"deployment": map[string]any{
				"type":        "string",
				"description": "Optional deployment name; empty returns every deployment.",
			},
			"repo": map[string]any{
				"type":        "string",
				"description": "Optional repository URL override.  When empty, the operator's config is read and the deployment's configured repo is used (or the first deployment's repo when none specified).",
			},
		},
	}
}

// ReadOnly reports that read_status does not mutate state.
func (readStatus) ReadOnly() bool { return true }

// Run shells out to `pg_hardstorage status` with --repo resolved from
// the caller's args or the operator's config, and returns the structured
// fleet status.
func (t *readStatus) Run(ctx context.Context, args map[string]any) (Result, error) {
	cmd := []string{"status"}
	dep, _ := args["deployment"].(string)
	if dep != "" {
		cmd = append(cmd, dep)
	}
	// `pg_hardstorage status` requires --repo.  Resolve from the
	// caller's args first, fall back to the operator's config —
	// without this, the CLI fails with `usage.missing_flag`
	// (pilot case C1: every cluster-state tool failed and the
	// model returned an empty answer).
	repo, _ := args["repo"].(string)
	if repo == "" {
		repo = repoResolver(dep)
	}
	if repo != "" {
		cmd = append(cmd, "--repo", repo)
	}
	body, err := t.runner.RunJSON(ctx, cmd...)
	if err != nil {
		return Result{}, err
	}
	return parseAsResult("status retrieved", body)
}

// ----- list_deployments -----

type listDeployments struct{ runner *CLIRunner }

// Name returns the tool identifier "list_deployments".
func (listDeployments) Name() string { return "list_deployments" }

// Description returns the model-facing summary of the tool's purpose.
func (listDeployments) Description() string {
	return "List every configured deployment (name, connection summary, repository URL, schedule)."
}

// Schema returns the JSON-schema-shaped argument descriptor.
func (listDeployments) Schema() map[string]any {
	return map[string]any{"type": "object"}
}

// ReadOnly reports that list_deployments does not mutate state.
func (listDeployments) ReadOnly() bool { return true }

// Run shells out to `pg_hardstorage deployment list` and returns the result.
func (t *listDeployments) Run(ctx context.Context, _ map[string]any) (Result, error) {
	body, err := t.runner.RunJSON(ctx, "deployment", "list")
	if err != nil {
		return Result{}, err
	}
	return parseAsResult("deployments listed", body)
}

// ----- list_backups -----

type listBackups struct{ runner *CLIRunner }

// Name returns the tool identifier "list_backups".
func (listBackups) Name() string { return "list_backups" }

// Description returns the model-facing summary of the tool's purpose.
func (listBackups) Description() string {
	return "List every backup for a deployment (backup_id, started_at, type, verified state, retention age)."
}

// Schema returns the JSON-schema-shaped argument descriptor.
func (listBackups) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"deployment": map[string]any{"type": "string", "description": "Deployment name."},
			"repo":       map[string]any{"type": "string", "description": "Optional repository URL override."},
		},
		"required": []string{"deployment"},
	}
}

// ReadOnly reports that list_backups does not mutate state.
func (listBackups) ReadOnly() bool { return true }

// Run shells out to `pg_hardstorage list <deployment>` with --repo
// resolved from the caller's args or the operator's config.
func (t *listBackups) Run(ctx context.Context, args map[string]any) (Result, error) {
	dep, _ := args["deployment"].(string)
	if dep == "" {
		return Result{}, errors.New("list_backups: deployment is required")
	}
	cmd := []string{"list", dep}
	// `list` requires --repo; resolve from arg or config.  Same
	// bug class as read_status (pilot case C1).
	repo, _ := args["repo"].(string)
	if repo == "" {
		repo = repoResolver(dep)
	}
	if repo != "" {
		cmd = append(cmd, "--repo", repo)
	}
	body, err := t.runner.RunJSON(ctx, cmd...)
	if err != nil {
		return Result{}, err
	}
	return parseAsResult(fmt.Sprintf("backups listed for %s", dep), body)
}

// ----- read_backup -----

type readBackup struct{ runner *CLIRunner }

// Name returns the tool identifier "read_backup".
func (readBackup) Name() string { return "read_backup" }

// Description returns the model-facing summary of the tool's purpose.
func (readBackup) Description() string {
	return "Show one backup's manifest details (size, file count, encryption ref, WAL range, timeline, attestation status)."
}

// Schema returns the JSON-schema-shaped argument descriptor.
func (readBackup) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"deployment": map[string]any{"type": "string", "description": "Deployment name."},
			"backup_id":  map[string]any{"type": "string", "description": "Backup ID; pass 'latest' to read the newest one."},
			"repo":       map[string]any{"type": "string", "description": "Optional repository URL override."},
		},
		"required": []string{"deployment", "backup_id"},
	}
}

// ReadOnly reports that read_backup does not mutate state.
func (readBackup) ReadOnly() bool { return true }

// Run shells out to `pg_hardstorage show <deployment> <backup-id>` with
// --repo resolved from the caller's args or the operator's config.
func (t *readBackup) Run(ctx context.Context, args map[string]any) (Result, error) {
	dep, _ := args["deployment"].(string)
	id, _ := args["backup_id"].(string)
	if dep == "" || id == "" {
		return Result{}, errors.New("read_backup: deployment and backup_id are required")
	}
	// `show <deployment> <backup-id>` requires --repo and takes
	// two positionals (deployment + backup id).  Resolve repo
	// from arg or config.  Same bug class as read_status
	// (pilot case C1).
	cmd := []string{"show", dep, id}
	repo, _ := args["repo"].(string)
	if repo == "" {
		repo = repoResolver(dep)
	}
	if repo != "" {
		cmd = append(cmd, "--repo", repo)
	}
	body, err := t.runner.RunJSON(ctx, cmd...)
	if err != nil {
		return Result{}, err
	}
	return parseAsResult(fmt.Sprintf("backup %s/%s loaded", dep, id), body)
}

// ----- read_repo_usage -----

type readRepoUsage struct{ runner *CLIRunner }

// Name returns the tool identifier "read_repo_usage".
func (readRepoUsage) Name() string { return "read_repo_usage" }

// Description returns the model-facing summary of the tool's purpose.
func (readRepoUsage) Description() string {
	return "Report repository usage (object count, bytes used, retention state, WORM mode)."
}

// Schema returns the JSON-schema-shaped argument descriptor.
func (readRepoUsage) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": map[string]any{"type": "string", "description": "Repository URL (e.g. s3://bucket/, file:///path/...). Required."},
		},
		"required": []string{"repo"},
	}
}

// ReadOnly reports that read_repo_usage does not mutate state.
func (readRepoUsage) ReadOnly() bool { return true }

// Run shells out to `pg_hardstorage repo usage <repo>` and returns the result.
func (t *readRepoUsage) Run(ctx context.Context, args map[string]any) (Result, error) {
	r, _ := args["repo"].(string)
	if r == "" {
		return Result{}, errors.New("read_repo_usage: repo URL is required")
	}
	body, err := t.runner.RunJSON(ctx, "repo", "usage", r)
	if err != nil {
		return Result{}, err
	}
	return parseAsResult("repo usage retrieved", body)
}

// ----- read_audit -----

type readAudit struct{ runner *CLIRunner }

// Name returns the tool identifier "read_audit".
func (readAudit) Name() string { return "read_audit" }

// Description returns the model-facing summary of the tool's purpose.
func (readAudit) Description() string {
	return "Search the hash-chained audit log. RBAC-scoped — the operator's tenant is enforced by the binary, not by this tool."
}

// Schema returns the JSON-schema-shaped argument descriptor.
func (readAudit) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":       map[string]any{"type": "string", "description": "Repository URL (required)."},
			"action":     map[string]any{"type": "string", "description": "Filter by action verb (e.g. 'kms.shred', 'restore.complete')."},
			"deployment": map[string]any{"type": "string", "description": "Filter by deployment name."},
			"since":      map[string]any{"type": "string", "description": "Filter by RFC3339 / natural-language start time."},
			"limit":      map[string]any{"type": "integer", "description": "Max entries to return (default 50)."},
		},
		"required": []string{"repo"},
	}
}

// ReadOnly reports that read_audit does not mutate state.
func (readAudit) ReadOnly() bool { return true }

// Run shells out to `pg_hardstorage audit search` with the requested
// filters and returns the matching entries.
func (t *readAudit) Run(ctx context.Context, args map[string]any) (Result, error) {
	r, _ := args["repo"].(string)
	if r == "" {
		return Result{}, errors.New("read_audit: repo URL is required")
	}
	cmd := []string{"audit", "search", "--repo", r}
	if a, _ := args["action"].(string); a != "" {
		cmd = append(cmd, "--action", a)
	}
	if d, _ := args["deployment"].(string); d != "" {
		cmd = append(cmd, "--deployment", d)
	}
	if s, _ := args["since"].(string); s != "" {
		cmd = append(cmd, "--since", s)
	}
	switch lim := args["limit"].(type) {
	case float64:
		cmd = append(cmd, "--limit", fmt.Sprintf("%d", int(lim)))
	case int:
		cmd = append(cmd, "--limit", fmt.Sprintf("%d", lim))
	}
	body, err := t.runner.RunJSON(ctx, cmd...)
	if err != nil {
		return Result{}, err
	}
	return parseAsResult("audit entries retrieved", body)
}

// ----- list_runbooks -----

type listRunbooks struct{}

// Name returns the tool identifier "list_runbooks".
func (listRunbooks) Name() string { return "list_runbooks" }

// Description returns the model-facing summary of the tool's purpose.
func (listRunbooks) Description() string {
	return "List every shipped disaster-recovery runbook (R1..R7) with its title. Use read_runbook to fetch the full body."
}

// Schema returns the JSON-schema-shaped argument descriptor.
func (listRunbooks) Schema() map[string]any { return map[string]any{"type": "object"} }

// ReadOnly reports that list_runbooks does not mutate state.
func (listRunbooks) ReadOnly() bool { return true }

// Run returns the bundled runbook index (ID + title) from the embedded corpus.
func (listRunbooks) Run(_ context.Context, _ map[string]any) (Result, error) {
	idx, err := docs.RunbookIndex()
	if err != nil {
		return Result{}, err
	}
	return Result{
		Summary: fmt.Sprintf("%d runbooks available", len(idx)),
		Body:    map[string]any{"runbooks": idx},
	}, nil
}

// ----- search_docs -----

type searchDocs struct{}

// Name returns the tool identifier "search_docs".
func (searchDocs) Name() string { return "search_docs" }

// Description returns the model-facing summary of the tool's purpose.
func (searchDocs) Description() string {
	return "Full-text search the bundled documentation (runbooks, CHANGELOG, README). Returns up to three excerpts per matching doc."
}

// Schema returns the JSON-schema-shaped argument descriptor.
func (searchDocs) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "Free-text query."},
		},
		"required": []string{"query"},
	}
}

// ReadOnly reports that search_docs does not mutate state.
func (searchDocs) ReadOnly() bool { return true }

// Run runs a full-text search against the bundled doc corpus and returns
// the matching entries with excerpts.
func (searchDocs) Run(_ context.Context, args map[string]any) (Result, error) {
	q, _ := args["query"].(string)
	if strings.TrimSpace(q) == "" {
		return Result{}, errors.New("search_docs: query is required")
	}
	matches, err := docs.Search(q)
	if err != nil {
		return Result{}, err
	}
	ids := make([]string, len(matches))
	for i, m := range matches {
		ids[i] = m.Doc.ID
	}
	return Result{
		Summary: fmt.Sprintf("%d matches: %s", len(matches), strings.Join(ids, ", ")),
		Body:    map[string]any{"matches": matches, "query": q},
	}, nil
}

// parseAsResult wraps a JSON body into a Result.  We decode to
// `any` rather than the typed Result struct so the LLM sees the
// full structure the CLI emitted (schema/result/generated_at +
// the typed body inside).
func parseAsResult(summary string, body []byte) (Result, error) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		// Surface the unparseable body anyway — the LLM may still
		// extract useful information from a malformed envelope
		// (e.g. a partial error message).
		return Result{
			Summary: summary + " (response was not JSON)",
			Body:    map[string]any{"raw": string(body), "parse_error": err.Error()},
		}, nil
	}
	return Result{Summary: summary, Body: v}, nil
}
