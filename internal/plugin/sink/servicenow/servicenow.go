// Package servicenow implements an output.Sink that creates /
// updates incidents in a ServiceNow instance via the Now Platform's
// Table API.  Closes the SPEC commitment for the `servicenow`
// sink ("incident management" pairing alongside jira / opsgenie /
// pagerduty in the operator-integration inventory).
//
// Configuration (YAML keys):
//
//	plugin: servicenow
//	config:
//	  instance_url: https://acme.service-now.com
//	  username:     ops_integration                   # for Basic auth
//	  password:     <PASSWORD>                        # for Basic auth
//	  bearer_token: <OAUTH_TOKEN>                     # alternative to user+pass
//	  category:     software                          # default
//	  assignment_group: db-ops                        # optional
//	  caller_id:    ops-bot                           # optional
//	  min_severity: error                             # default
//	  ticket_strategy: dedupe_by_subject              # dedupe_by_subject | always_new
//	  active_states: [1, 2, 3]                        # states that count as "still open" for dedup
//
// dedupe_by_subject (default) reuses an existing open incident
// whose `short_description` matches the event's identity tuple
// (component / op / deployment).  Recurring failures append
// `work_notes` instead of opening a new incident — the ServiceNow
// equivalent of JIRA's "comment on the same ticket" posture.
//
// always_new opens a fresh incident per event.  Useful when every
// occurrence has independent significance (audit-style emission).
//
// ServiceNow severity model: incidents carry `urgency` and `impact`,
// each on a 1 (highest) → 3 (lowest) scale.  We map RFC 5424 levels:
//
//	emergency / alert / critical → urgency=1, impact=1  (P1 incident)
//	error                        → urgency=2, impact=2  (P2 incident)
//	warning                      → urgency=3, impact=2  (P3 incident)
//	notice / info / debug        → urgency=3, impact=3  (filtered by min_severity)
//
// The instance still computes its own derived `priority` from the
// (urgency, impact) tuple via its priority-lookup table — different
// instances rank P1/P2/P3 differently, so we surface only the inputs.
package servicenow

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func init() {
	output.DefaultSinkRegistry.Register("servicenow", NewFromSpec)
}

// TicketStrategy controls per-event behaviour.
type TicketStrategy string

const (
	// StrategyDedupe appends a Work Note to an existing open incident
	// with the same subject instead of opening a duplicate.
	StrategyDedupe TicketStrategy = "dedupe_by_subject"
	// StrategyAlwaysNew opens a fresh incident for every event,
	// matching change-control workflows that require one record per
	// occurrence.
	StrategyAlwaysNew TicketStrategy = "always_new"
)

// DefaultActiveStates is the set of `state` values that we treat
// as "still open" for dedup-by-subject lookups.  ServiceNow's
// out-of-the-box incident state model is:
//
//	1 = New          2 = In Progress    3 = On Hold
//	6 = Resolved     7 = Closed         8 = Canceled
//
// 1/2/3 are open; 6/7/8 are closed (we don't append to closed
// incidents).  Operators who customised the model override via the
// `active_states` config key.
var DefaultActiveStates = []int{1, 2, 3}

// Sink creates / updates ServiceNow incidents.
type Sink struct {
	name            string
	baseURL         string
	authHeader      string
	category        string
	assignmentGroup string
	callerID        string
	minSeverity     output.Severity
	strategy        TicketStrategy
	activeStates    []int

	httpClient *http.Client
	mu         sync.Mutex
	closed     bool
}

// NewFromSpec is the SinkBuilder.
func NewFromSpec(spec output.SinkSpec) (output.Sink, error) {
	instanceURL, err := output.SinkConfigString(spec.Config, "instance_url")
	if err != nil {
		return nil, err
	}
	if instanceURL == "" {
		return nil, errors.New("servicenow: config.instance_url is required")
	}
	if _, err := url.Parse(instanceURL); err != nil {
		return nil, fmt.Errorf("servicenow: parse instance_url: %w", err)
	}
	if err := airgap.Default().EndpointAllowed(instanceURL); err != nil {
		return nil, fmt.Errorf("servicenow: %w", err)
	}

	authHeader, err := buildAuthHeader(spec.Config)
	if err != nil {
		return nil, err
	}

	category, err := output.SinkConfigStringDefault(spec.Config, "category", "software")
	if err != nil {
		return nil, err
	}
	assignmentGroup, _ := output.SinkConfigString(spec.Config, "assignment_group")
	callerID, _ := output.SinkConfigString(spec.Config, "caller_id")

	minSevStr, err := output.SinkConfigStringDefault(spec.Config, "min_severity", "error")
	if err != nil {
		return nil, err
	}
	minSev, perr := output.ParseSeverity(minSevStr)
	if perr != nil {
		return nil, fmt.Errorf("servicenow: %w", perr)
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
		return nil, fmt.Errorf("servicenow: unknown ticket_strategy %q (allowed: %s, %s)",
			stratStr, StrategyDedupe, StrategyAlwaysNew)
	}

	activeStates, err := parseActiveStates(spec.Config)
	if err != nil {
		return nil, err
	}

	return &Sink{
		name:            spec.Name,
		baseURL:         strings.TrimRight(instanceURL, "/"),
		authHeader:      authHeader,
		category:        category,
		assignmentGroup: assignmentGroup,
		callerID:        callerID,
		minSeverity:     minSev,
		strategy:        strategy,
		activeStates:    activeStates,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}, nil
}

// buildAuthHeader picks one of the supported auth shapes:
//
//   - username + password → HTTP Basic
//   - bearer_token        → Bearer (OAuth)
//
// Operator must supply exactly one set; both or neither is a
// configuration error.
func buildAuthHeader(cfg map[string]any) (string, error) {
	username, _ := output.SinkConfigString(cfg, "username")
	password, _ := output.SinkConfigString(cfg, "password")
	bearer, _ := output.SinkConfigString(cfg, "bearer_token")

	hasBasic := username != "" || password != ""
	hasBearer := bearer != ""

	if hasBasic && hasBearer {
		return "", errors.New("servicenow: cannot combine username/password (Basic) and bearer_token; pick one")
	}
	if hasBasic {
		if username == "" || password == "" {
			return "", errors.New("servicenow: Basic auth needs both username and password")
		}
		raw := username + ":" + password
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw)), nil
	}
	if hasBearer {
		return "Bearer " + bearer, nil
	}
	return "", errors.New("servicenow: auth required (username + password, OR bearer_token)")
}

// parseActiveStates parses the `active_states` config key.  Accepts
// []int, []float64 (YAML's default), or []any with int-shaped
// elements.  Defaults to DefaultActiveStates.
func parseActiveStates(cfg map[string]any) ([]int, error) {
	v, ok := cfg["active_states"]
	if !ok {
		return append([]int(nil), DefaultActiveStates...), nil
	}
	out := []int{}
	switch xs := v.(type) {
	case []int:
		out = append(out, xs...)
	case []any:
		for _, item := range xs {
			switch n := item.(type) {
			case int:
				out = append(out, n)
			case int64:
				out = append(out, int(n))
			case float64:
				out = append(out, int(n))
			case string:
				i, err := strconv.Atoi(n)
				if err != nil {
					return nil, fmt.Errorf("servicenow: active_states item %q not an int: %w", n, err)
				}
				out = append(out, i)
			default:
				return nil, fmt.Errorf("servicenow: active_states item type %T not supported", item)
			}
		}
	default:
		return nil, fmt.Errorf("servicenow: active_states must be a list of ints (got %T)", v)
	}
	if len(out) == 0 {
		return nil, errors.New("servicenow: active_states must be non-empty")
	}
	return out, nil
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
		return errors.New("servicenow: sink closed")
	}
	s.mu.Unlock()

	if !ev.Severity.AtLeast(s.minSeverity) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if s.strategy == StrategyDedupe {
		sysID, number, err := s.findExistingIncident(ctx, ev)
		if err != nil {
			return err
		}
		if sysID != "" {
			return s.appendWorkNotes(ctx, sysID, number, ev)
		}
	}
	return s.createIncident(ctx, ev)
}

// Close implements output.Sink.
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// dedupSummary builds the canonical short_description for dedupe
// matching.  Identical events → identical summaries → one incident.
func (s *Sink) dedupSummary(ev *output.Event) string {
	parts := []string{"[pg_hardstorage] " + ev.Op}
	if ev.Subject.Deployment != "" {
		parts = append(parts, "deployment="+ev.Subject.Deployment)
	}
	return strings.Join(parts, " · ")
}

// findExistingIncident searches the incident table for an open
// incident whose `short_description` matches the dedup summary.
// Returns (sys_id, number, nil) for the match or ("", "", nil) for
// none.
//
// ServiceNow query syntax: `^` joins clauses (AND).  Active states
// use `stateIN1,2,3` (no spaces).  String equality on
// `short_description` is the literal `=` operator; the value must
// be ServiceNow-escaped (we strip `^` since it's the AND token —
// allowing it would fold our intended one-clause query into many).
func (s *Sink) findExistingIncident(ctx context.Context, ev *output.Event) (sysID, number string, _ error) {
	summary := s.dedupSummary(ev)
	stateClause := stateInClause(s.activeStates)
	query := fmt.Sprintf("short_description=%s^%s",
		serviceNowEscape(summary), stateClause)
	q := url.Values{}
	q.Set("sysparm_query", query)
	q.Set("sysparm_fields", "sys_id,number")
	q.Set("sysparm_limit", "1")
	endpoint := s.baseURL + "/api/now/table/incident?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", fmt.Errorf("servicenow: build search request: %w", err)
	}
	req.Header.Set("Authorization", s.authHeader)
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("servicenow: search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", fmt.Errorf("servicenow: search status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Result []struct {
			SysID  string `json:"sys_id"`
			Number string `json:"number"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("servicenow: decode search: %w", err)
	}
	if len(out.Result) == 0 {
		return "", "", nil
	}
	return out.Result[0].SysID, out.Result[0].Number, nil
}

// createIncident POSTs a new incident to the table API.
func (s *Sink) createIncident(ctx context.Context, ev *output.Event) error {
	urgency, impact := severityToUrgencyImpact(ev.Severity)
	body := map[string]any{
		"short_description": s.dedupSummary(ev),
		"description":       formatBody(ev),
		"category":          s.category,
		"urgency":           urgency,
		"impact":            impact,
	}
	if s.assignmentGroup != "" {
		body["assignment_group"] = s.assignmentGroup
	}
	if s.callerID != "" {
		body["caller_id"] = s.callerID
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("servicenow: marshal create: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.baseURL+"/api/now/table/incident", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("servicenow: build create request: %w", err)
	}
	req.Header.Set("Authorization", s.authHeader)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("servicenow: create: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("servicenow: create status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// appendWorkNotes PATCHes the existing incident's `work_notes` field
// (audit-trail comment).  ServiceNow appends; our payload is the
// new note line.
func (s *Sink) appendWorkNotes(ctx context.Context, sysID, _ string, ev *output.Event) error {
	body := map[string]any{
		"work_notes": "[pg_hardstorage] " + formatBody(ev),
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("servicenow: marshal work_notes: %w", err)
	}
	endpoint := s.baseURL + "/api/now/table/incident/" + url.PathEscape(sysID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("servicenow: build work_notes request: %w", err)
	}
	req.Header.Set("Authorization", s.authHeader)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("servicenow: work_notes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("servicenow: work_notes status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// severityToUrgencyImpact maps the RFC 5424 level to ServiceNow's
// (urgency, impact) tuple.  Both 1..3 with 1 = highest.
func severityToUrgencyImpact(s output.Severity) (urgency, impact int) {
	switch {
	case s.AtLeast(output.SeverityCritical):
		return 1, 1
	case s.AtLeast(output.SeverityError):
		return 2, 2
	case s.AtLeast(output.SeverityWarning):
		return 3, 2
	default:
		return 3, 3
	}
}

// stateInClause builds `stateIN1,2,3` for ServiceNow's query syntax.
// Order is preserved (purely for the test-asserted form; semantically
// any order works).
func stateInClause(states []int) string {
	parts := make([]string, 0, len(states))
	for _, s := range states {
		parts = append(parts, strconv.Itoa(s))
	}
	return "stateIN" + strings.Join(parts, ",")
}

// formatBody renders the event into a compact body string.  Same
// shape as the jira sink for operator familiarity.
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

// serviceNowEscape strips the two characters that fold a query
// clause:
//
//   - `^` is the AND-clause separator; allowing it lets a
//     malicious value inject extra clauses (e.g.
//     `short_description=foo^state=999` matches differently).
//   - `=` is the operator; allowing it on the right-hand side
//     lets a value masquerade as a new clause.
//
// We prefer stripping over backslash-escape because ServiceNow's
// query parser does not have a uniform escape syntax across versions.
// The cost is that summaries containing literal `^` or `=` get
// those characters dropped — acceptable for the small set of
// fields we feed in (component, op, deployment), which are
// pg_hardstorage-emitted identifiers, not free-form user input.
func serviceNowEscape(s string) string {
	s = strings.ReplaceAll(s, "^", " ")
	s = strings.ReplaceAll(s, "=", " ")
	return s
}
