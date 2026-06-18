// Package opsgenie implements an output.Sink that creates alerts in
// Atlassian Opsgenie via the Alert API v2.
//
// Configuration (YAML keys):
//
//	plugin: opsgenie
//	config:
//	  api_key: <secret>                      # required
//	  api_url: https://api.opsgenie.com      # default; EU shard: https://api.eu.opsgenie.com
//	  teams: ["ops"]                         # optional; one or more team names
//	  tags: ["pg_hardstorage", "automation"] # optional
//	  source: "pg_hardstorage@db1"           # optional; default: "pg_hardstorage"
//	  min_severity: error                    # default: error (Opsgenie wakes people)
//
// Severity → Opsgenie priority mapping:
//
//	emergency, alert, critical → P1
//	error                      → P2
//	warning                    → P3
//	notice                     → P4
//	info, debug                → P5
//
// Dedup: each event uses a deterministic `alias` derived from
// (component, op, deployment, backup_id). Opsgenie auto-aggregates
// alerts sharing an alias within an open window — same logical
// failure firing repeatedly resolves to ONE alert; the same operator
// who acknowledges "wal lag" once doesn't get re-paged for the same
// condition every time the agent ticks.
package opsgenie

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func init() {
	output.DefaultSinkRegistry.Register("opsgenie", NewFromSpec)
}

// DefaultAPIURL is the global Opsgenie endpoint. Operators on the EU
// shard set api_url explicitly to https://api.eu.opsgenie.com — same
// posture as the PagerDuty sink's Events-API constant: the URL is
// canonical, hard-coded as a default, overridable for the rare
// alternative shards / on-prem deployments.
const DefaultAPIURL = "https://api.opsgenie.com"

// Sink fires Opsgenie create-alert calls.
type Sink struct {
	name        string
	apiURL      string
	apiKey      string
	teams       []string
	tags        []string
	source      string
	minSeverity output.Severity

	httpClient *http.Client
	mu         sync.Mutex
	closed     bool
}

// NewFromSpec is the SinkBuilder.
func NewFromSpec(spec output.SinkSpec) (output.Sink, error) {
	apiKey, err := output.SinkConfigString(spec.Config, "api_key")
	if err != nil {
		return nil, err
	}
	if apiKey == "" {
		return nil, errors.New("opsgenie: config.api_key is required")
	}
	apiURL, err := output.SinkConfigStringDefault(spec.Config, "api_url", DefaultAPIURL)
	if err != nil {
		return nil, err
	}
	if err := airgap.Default().EndpointAllowed(apiURL); err != nil {
		return nil, fmt.Errorf("opsgenie: %w", err)
	}
	source, err := output.SinkConfigStringDefault(spec.Config, "source", "pg_hardstorage")
	if err != nil {
		return nil, err
	}
	minSevStr, err := output.SinkConfigStringDefault(spec.Config, "min_severity", "error")
	if err != nil {
		return nil, err
	}
	minSev, perr := output.ParseSeverity(minSevStr)
	if perr != nil {
		return nil, fmt.Errorf("opsgenie: %w", perr)
	}

	teams, err := readStringList(spec.Config, "teams")
	if err != nil {
		return nil, err
	}
	tags, err := readStringList(spec.Config, "tags")
	if err != nil {
		return nil, err
	}

	return &Sink{
		name:        spec.Name,
		apiURL:      strings.TrimRight(apiURL, "/"),
		apiKey:      apiKey,
		teams:       teams,
		tags:        tags,
		source:      source,
		minSeverity: minSev,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// readStringList accepts a YAML list-of-strings or a single-string
// shorthand. Empty/absent → nil.
func readStringList(cfg map[string]any, key string) ([]string, error) {
	v, ok := cfg[key]
	if !ok {
		return nil, nil
	}
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		if x == "" {
			return nil, nil
		}
		return []string{x}, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("opsgenie: config.%s contains non-string %T", key, item)
			}
			out = append(out, s)
		}
		return out, nil
	case []string:
		return append([]string(nil), x...), nil
	}
	return nil, fmt.Errorf("opsgenie: config.%s: expected list of strings, got %T", key, v)
}

// Name implements output.Sink.
func (s *Sink) Name() string { return s.name }

// Open implements output.Sink.
func (s *Sink) Open(_ context.Context, _ map[string]any) error { return nil }

// Close implements output.Sink.
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Emit implements output.Sink.
func (s *Sink) Emit(ctx context.Context, ev *output.Event) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("opsgenie: sink closed")
	}
	s.mu.Unlock()

	if !ev.Severity.AtLeast(s.minSeverity) {
		return nil
	}
	// Pre-Emit ctx check (same posture as the syslog / email sink fixes).
	if err := ctx.Err(); err != nil {
		return err
	}

	payload := s.buildPayload(ev)
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("opsgenie: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.apiURL+"/v2/alerts", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("opsgenie: build request: %w", err)
	}
	req.Header.Set("Authorization", "GenieKey "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("opsgenie: post: %w", err)
	}
	defer resp.Body.Close()
	// Opsgenie returns 202 Accepted for queued alert creation. Treat
	// any 2xx as success; everything else surfaces with the body so
	// the operator sees the API's reason.
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("opsgenie: status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// alertPayload is the Opsgenie create-alert body. Field names match
// the Alert API v2 spec; only fields we populate are declared.
type alertPayload struct {
	Message     string         `json:"message"`
	Alias       string         `json:"alias"`
	Description string         `json:"description,omitempty"`
	Source      string         `json:"source,omitempty"`
	Priority    string         `json:"priority"`
	Tags        []string       `json:"tags,omitempty"`
	Responders  []responder    `json:"responders,omitempty"`
	Details     map[string]any `json:"details,omitempty"`
	Note        string         `json:"note,omitempty"`
}

type responder struct {
	Type string `json:"type"`           // "team" | "user" | "escalation" | "schedule"
	Name string `json:"name,omitempty"` // when addressing by name
}

func (s *Sink) buildPayload(ev *output.Event) alertPayload {
	p := alertPayload{
		Message:     messageFor(ev),
		Alias:       aliasFor(ev),
		Description: descriptionFor(ev),
		Source:      s.source,
		Priority:    mapPriority(ev.Severity),
		Tags:        append([]string(nil), s.tags...),
		Details:     detailsFor(ev),
	}
	for _, t := range s.teams {
		p.Responders = append(p.Responders, responder{Type: "team", Name: t})
	}
	if ev.Suggestion != nil && ev.Suggestion.DocURL != "" {
		// Opsgenie has no first-class "runbook URL" field on the
		// alert; we put it in Note, which renders as an alert note
		// in the UI.
		p.Note = "Runbook: " + ev.Suggestion.DocURL
	}
	return p
}

// messageFor produces the operator-facing alert headline. ≤130 chars
// per Opsgenie's API guide; we stay conservative.
func messageFor(ev *output.Event) string {
	parts := []string{strings.ToUpper(ev.SeverityName), ev.Component + "/" + ev.Op}
	if ev.Subject.Deployment != "" {
		parts = append(parts, "deployment="+ev.Subject.Deployment)
	}
	s := strings.Join(parts, " · ")
	const maxLen = 130
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return s
}

// aliasFor produces a deterministic dedup alias from the event's
// identity tuple. Same logical failure → same alias → Opsgenie
// aggregates rather than paging the on-call repeatedly.
//
// Hashed so the alias fits Opsgenie's 512-char alias bound and so
// surface-level format changes upstream don't collide unexpectedly.
func aliasFor(ev *output.Event) string {
	tuple := strings.Join([]string{
		ev.Component, ev.Op,
		ev.Subject.Deployment, ev.Subject.BackupID,
	}, "\x00")
	sum := sha256.Sum256([]byte(tuple))
	return "pgh-" + hex.EncodeToString(sum[:])[:32]
}

// descriptionFor renders the long-form alert body — what the on-call
// operator reads after the headline. Mirrors the jira / email shape.
func descriptionFor(ev *output.Event) string {
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
			fmt.Fprintf(bw, "\nSuggestion: %s\n", ev.Suggestion.Human)
		}
		if ev.Suggestion.Command != "" {
			fmt.Fprintf(bw, "Command: %s\n", ev.Suggestion.Command)
		}
	}
	return strings.TrimRight(bw.String(), "\n")
}

// detailsFor packs the event's Subject + Body into Opsgenie's
// `details` map, which renders as a key/value table on the alert
// detail page.
func detailsFor(ev *output.Event) map[string]any {
	out := map[string]any{}
	if ev.Subject.Tenant != "" {
		out["tenant"] = ev.Subject.Tenant
	}
	if ev.Subject.Timeline != 0 {
		// Opsgenie's `details` only accepts string→string in the
		// v2 spec; coerce numeric values.
		out["timeline"] = fmt.Sprintf("%d", ev.Subject.Timeline)
	}
	if ev.Subject.LSN != "" {
		out["lsn"] = ev.Subject.LSN
	}
	if ev.Suggestion != nil && ev.Suggestion.Command != "" {
		out["suggested_command"] = ev.Suggestion.Command
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mapPriority collapses our 8-level RFC 5424 ladder onto Opsgenie's
// 5-level P1–P5. Conservative: error stays at P2, anything more
// severe (alert / critical / emergency) becomes P1 (the highest).
func mapPriority(s output.Severity) string {
	switch {
	case s <= output.SeverityCritical:
		return "P1"
	case s == output.SeverityError:
		return "P2"
	case s == output.SeverityWarning:
		return "P3"
	case s == output.SeverityNotice:
		return "P4"
	default:
		return "P5"
	}
}
