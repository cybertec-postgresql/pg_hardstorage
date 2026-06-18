// Package jira implements an output.Sink that creates / updates
// JIRA issues in response to events.
//
// Configuration (YAML keys):
//
//	plugin: jira
//	config:
//	  base_url: https://acme.atlassian.net
//	  project: OPS
//	  issue_type: Incident             # default: Incident
//	  email: ops@acme.com              # for basic auth (cloud)
//	  api_token: <ATLASSIAN_API_TOKEN> # for basic auth (cloud)
//	  bearer_token: <PAT>              # for self-hosted PAT (alternative)
//	  min_severity: error              # default: error
//	  ticket_strategy: dedupe_by_subject  # dedupe_by_subject | always_new
//	  labels: ["pg-hardstorage", "automation"]
//
// dedupe_by_subject (default) reuses an existing open ticket whose
// summary matches the event's identity tuple (deployment + op).
// Recurring failures append a comment instead of opening a new
// ticket — exactly the SPEC's "one ticket per recurring failure"
// posture.
//
// always_new opens a fresh ticket per event. Useful for audit
// emission where every event has independent significance.
package jira

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func init() {
	output.DefaultSinkRegistry.Register("jira", NewFromSpec)
}

// TicketStrategy controls per-event behavior.
type TicketStrategy string

const (
	// StrategyDedupe collapses repeated subjects into a single open
	// ticket so a flapping alert doesn't burn the team's queue.
	StrategyDedupe TicketStrategy = "dedupe_by_subject"
	// StrategyAlwaysNew creates a fresh ticket for every event;
	// suitable for change-control workflows that require one ticket
	// per occurrence.
	StrategyAlwaysNew TicketStrategy = "always_new"
)

// Sink creates JIRA issues. The full ticket-state-machine
// (transitioning, resolving) lands when the audit slice can
// correlate "this resolution event resolves that ticket."
type Sink struct {
	name        string
	baseURL     string
	project     string
	issueType   string
	authHeader  string
	minSeverity output.Severity
	strategy    TicketStrategy
	labels      []string

	httpClient *http.Client
	mu         sync.Mutex
	closed     bool
}

// NewFromSpec is the SinkBuilder.
func NewFromSpec(spec output.SinkSpec) (output.Sink, error) {
	baseURL, err := output.SinkConfigString(spec.Config, "base_url")
	if err != nil {
		return nil, err
	}
	if baseURL == "" {
		return nil, errors.New("jira: config.base_url is required")
	}
	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("jira: parse base_url: %w", err)
	}
	if err := airgap.Default().EndpointAllowed(baseURL); err != nil {
		return nil, fmt.Errorf("jira: %w", err)
	}
	project, err := output.SinkConfigString(spec.Config, "project")
	if err != nil {
		return nil, err
	}
	if project == "" {
		return nil, errors.New("jira: config.project is required")
	}
	issueType, err := output.SinkConfigStringDefault(spec.Config, "issue_type", "Incident")
	if err != nil {
		return nil, err
	}

	authHeader, err := buildAuthHeader(spec.Config)
	if err != nil {
		return nil, err
	}

	minSevStr, err := output.SinkConfigStringDefault(spec.Config, "min_severity", "error")
	if err != nil {
		return nil, err
	}
	minSev, perr := output.ParseSeverity(minSevStr)
	if perr != nil {
		return nil, fmt.Errorf("jira: %w", perr)
	}

	stratStr, err := output.SinkConfigStringDefault(spec.Config, "ticket_strategy", string(StrategyDedupe))
	if err != nil {
		return nil, err
	}
	var strategy TicketStrategy
	switch TicketStrategy(stratStr) {
	case StrategyDedupe, StrategyAlwaysNew:
		strategy = TicketStrategy(stratStr)
	default:
		return nil, fmt.Errorf("jira: unknown ticket_strategy %q (allowed: %s, %s)",
			stratStr, StrategyDedupe, StrategyAlwaysNew)
	}

	var labels []string
	if v, ok := spec.Config["labels"]; ok {
		switch ls := v.(type) {
		case []any:
			for _, item := range ls {
				if s, ok := item.(string); ok {
					labels = append(labels, s)
				}
			}
		case []string:
			labels = ls
		default:
			return nil, fmt.Errorf("jira: config.labels must be a list of strings (got %T)", v)
		}
	}

	return &Sink{
		name:        spec.Name,
		baseURL:     strings.TrimRight(baseURL, "/"),
		project:     project,
		issueType:   issueType,
		authHeader:  authHeader,
		minSeverity: minSev,
		strategy:    strategy,
		labels:      labels,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}, nil
}

// buildAuthHeader picks one of the supported auth shapes:
//
//   - email + api_token  → HTTP Basic (Atlassian Cloud)
//   - bearer_token       → Bearer (self-hosted PAT)
//
// Operator must supply exactly one set; both or neither is a
// configuration error.
func buildAuthHeader(cfg map[string]any) (string, error) {
	email, _ := output.SinkConfigString(cfg, "email")
	apiToken, _ := output.SinkConfigString(cfg, "api_token")
	bearer, _ := output.SinkConfigString(cfg, "bearer_token")

	hasBasic := email != "" || apiToken != ""
	hasBearer := bearer != ""

	if hasBasic && hasBearer {
		return "", errors.New("jira: cannot combine email/api_token (Basic) and bearer_token; pick one")
	}
	if hasBasic {
		if email == "" || apiToken == "" {
			return "", errors.New("jira: Basic auth needs both email and api_token")
		}
		raw := email + ":" + apiToken
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw)), nil
	}
	if hasBearer {
		return "Bearer " + bearer, nil
	}
	return "", errors.New("jira: auth required (email + api_token, OR bearer_token)")
}

// Name implements output.Sink.
func (s *Sink) Name() string { return s.name }

// Open implements output.Sink.
func (s *Sink) Open(_ context.Context, _ map[string]any) error { return nil }

// Emit implements output.Sink.
func (s *Sink) Emit(ctx context.Context, ev *output.Event) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("jira: sink closed")
	}
	s.mu.Unlock()

	if !ev.Severity.AtLeast(s.minSeverity) {
		return nil
	}
	// Pre-Emit ctx check (consistent with syslog / email / opsgenie /
	// pagerduty / slack / webhook). The dedupe-by-subject path makes
	// TWO HTTP calls (search + comment-or-create); an already-cancelled
	// ctx must bail before the first.
	if err := ctx.Err(); err != nil {
		return err
	}

	if s.strategy == StrategyDedupe {
		key, err := s.findExistingIssue(ctx, ev)
		if err != nil {
			return err
		}
		if key != "" {
			return s.appendComment(ctx, key, ev)
		}
	}
	return s.createIssue(ctx, ev)
}

// Close implements output.Sink.
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// dedupSummary builds the canonical issue summary for dedupe-by-
// subject. Identical events → identical summaries → one issue.
func (s *Sink) dedupSummary(ev *output.Event) string {
	parts := []string{"[pg_hardstorage] " + ev.Op}
	if ev.Subject.Deployment != "" {
		parts = append(parts, "deployment="+ev.Subject.Deployment)
	}
	return strings.Join(parts, " · ")
}

// findExistingIssue searches JQL for an open issue matching the
// dedupe summary. Returns the issue key (e.g. "OPS-1234") or "".
//
// We use exact-summary matching via JQL `summary ~ "<term>"` rather
// than label-based dedup because labels can be edited by humans;
// the summary is stable.
//
// JQL string escaping: the project name is operator-set in config
// (so attacker-controlled only for an attacker who already owns the
// config), and the summary derives from ev.Op + ev.Subject.Deployment
// — both operator-emitted but not necessarily sanitized. We funnel
// both through jiraEscape() to avoid relying on Go's `%q` happening
// to be JQL-compatible (it usually is — the escape vocabularies
// overlap — but an explicit escape makes the intent obvious and
// survives a future change in either spec).
func (s *Sink) findExistingIssue(ctx context.Context, ev *output.Event) (string, error) {
	summary := s.dedupSummary(ev)
	jql := fmt.Sprintf(`project = "%s" AND statusCategory != Done AND summary ~ "%s"`,
		jiraEscape(s.project), jiraEscape(summary))
	q := url.Values{}
	q.Set("jql", jql)
	q.Set("fields", "summary")
	q.Set("maxResults", "1")
	endpoint := s.baseURL + "/rest/api/3/search?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("jira: build search request: %w", err)
	}
	req.Header.Set("Authorization", s.authHeader)
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("jira: search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("jira: search status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Issues []struct {
			Key string `json:"key"`
		} `json:"issues"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("jira: decode search: %w", err)
	}
	if len(out.Issues) == 0 {
		return "", nil
	}
	return out.Issues[0].Key, nil
}

// createIssue opens a new ticket. Body is ADF-shaped (Atlassian
// Document Format) — JIRA Cloud's REST API v3 mandates ADF for the
// description field.
func (s *Sink) createIssue(ctx context.Context, ev *output.Event) error {
	body := map[string]any{
		"fields": map[string]any{
			"project":     map[string]any{"key": s.project},
			"summary":     s.dedupSummary(ev),
			"issuetype":   map[string]any{"name": s.issueType},
			"description": adfDescription(ev),
		},
	}
	if len(s.labels) > 0 {
		body["fields"].(map[string]any)["labels"] = s.labels
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("jira: marshal create: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.baseURL+"/rest/api/3/issue", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("jira: build create request: %w", err)
	}
	req.Header.Set("Authorization", s.authHeader)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("jira: create: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("jira: create status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// appendComment posts to /issue/{key}/comment to add a note to the
// existing dedup-matched ticket.
func (s *Sink) appendComment(ctx context.Context, issueKey string, ev *output.Event) error {
	payload, err := json.Marshal(map[string]any{
		"body": adfComment(ev),
	})
	if err != nil {
		return fmt.Errorf("jira: marshal comment: %w", err)
	}
	endpoint := fmt.Sprintf("%s/rest/api/3/issue/%s/comment", s.baseURL, issueKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("jira: build comment request: %w", err)
	}
	req.Header.Set("Authorization", s.authHeader)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("jira: comment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("jira: comment status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// adfDescription / adfComment build minimal ADF documents. ADF is a
// rich-text format; we use the simplest possible "paragraph with
// some text" shape since operators read these in JIRA's web UI.
func adfDescription(ev *output.Event) map[string]any {
	return adfDoc(formatBody(ev))
}

func adfComment(ev *output.Event) map[string]any {
	return adfDoc(formatBody(ev))
}

func adfDoc(text string) map[string]any {
	return map[string]any{
		"type":    "doc",
		"version": 1,
		"content": []any{
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{
						"type": "text",
						"text": text,
					},
				},
			},
		},
	}
}

// formatBody renders the event into a single string body for the
// ADF document. Compact and operator-readable; the structured detail
// is in the linked Sink-stream events for anyone who wants the JSON.
func formatBody(ev *output.Event) string {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "Severity: %s\n", strings.ToUpper(ev.SeverityName))
	fmt.Fprintf(bw, "Component: %s\n", ev.Component)
	fmt.Fprintf(bw, "Op: %s\n", ev.Op)
	fmt.Fprintf(bw, "At: %s\n", ev.GeneratedAt.UTC().Format(time.RFC3339))
	if ev.Subject.Deployment != "" {
		fmt.Fprintf(bw, "Deployment: %s\n", ev.Subject.Deployment)
	}
	if ev.Subject.BackupID != "" {
		fmt.Fprintf(bw, "Backup: %s\n", ev.Subject.BackupID)
	}
	if ev.Subject.Timeline != 0 {
		fmt.Fprintf(bw, "Timeline: %d\n", ev.Subject.Timeline)
	}
	if ev.Subject.LSN != "" {
		fmt.Fprintf(bw, "LSN: %s\n", ev.Subject.LSN)
	}
	if ev.Body != nil {
		bs, _ := json.Marshal(ev.Body)
		fmt.Fprintf(bw, "Body: %s\n", bs)
	}
	if ev.Suggestion != nil {
		if ev.Suggestion.Human != "" {
			fmt.Fprintf(bw, "Suggestion: %s\n", ev.Suggestion.Human)
		}
		if ev.Suggestion.Command != "" {
			fmt.Fprintf(bw, "Command: %s\n", ev.Suggestion.Command)
		}
		if ev.Suggestion.DocURL != "" {
			fmt.Fprintf(bw, "Runbook: %s\n", ev.Suggestion.DocURL)
		}
	}
	return strings.TrimRight(bw.String(), "\n")
}

// jiraEscape escapes a string for safe inclusion inside a
// double-quoted JQL literal. The two characters that can break
// out of the quoted string are `"` (closes the literal) and `\`
// (introduces an escape sequence) — escape both with a leading
// backslash. JQL's full grammar also reserves Lucene-style
// metacharacters for the `~` operator's text matching (`+`, `-`,
// `*`, `?`, etc.), but those affect ranking inside the `~` query
// rather than letting an attacker break out of the string —
// they're not security-relevant here. Escaping `"` and `\` is the
// minimum sufficient set to prevent JQL injection.
//
// Go's `%q` happens to produce JQL-compatible escapes for the
// 99% case (it escapes `"` as `\"` and `\` as `\\`), but `%q`
// can also emit `\xNN` and `\uNNNN` Unicode-escape forms that
// JIRA Cloud's parser may or may not accept. This helper sticks
// to the two characters that matter for safety.
func jiraEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
