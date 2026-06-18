// Package pagerduty implements an output.Sink that fires events to
// PagerDuty's Events API v2.
//
// Configuration (YAML keys):
//
//	plugin: pagerduty
//	config:
//	  routing_key: "<PD-integration-routing-key>"
//	  source: "pg_hardstorage@db1"     # optional; default: "pg_hardstorage"
//	  min_severity: error              # default: error (PD is for waking people)
//	  client: "pg_hardstorage"         # optional; appears in incidents
//	  client_url: "https://..."        # optional
//
// Severity → PD severity mapping:
//
//	emergency, alert, critical → "critical"
//	error                      → "error"
//	warning                    → "warning"
//	notice, info, debug        → "info"
//
// Dedup: each event uses a deterministic dedup_key derived from
// (component, op, deployment, backup_id). Same logical failure firing
// repeatedly resolves to ONE PD incident; the same operator who
// silences "wal lag" once doesn't get re-paged for the same
// condition every time the agent ticks.
package pagerduty

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
	output.DefaultSinkRegistry.Register("pagerduty", NewFromSpec)
}

// EventsAPIv2URL is the canonical PagerDuty endpoint. Hard-coded
// because PD doesn't run S3-compatible mirrors; if ever a private
// PD-on-prem-style deployment shows up, an `endpoint` config key is
// the right shape.
const EventsAPIv2URL = "https://events.pagerduty.com/v2/enqueue"

// apiURL is the URL Emit POSTs to. Production reads
// EventsAPIv2URL; tests redirect via the OverrideEventsAPIv2URL
// hook in test_hooks_test.go. We don't expose this field publicly
// because it's an implementation detail — every operator-visible
// PD URL goes through the const.
var apiURL = EventsAPIv2URL

// Sink fires PD Events-API-v2 trigger / acknowledge / resolve actions.
// v0.1 ships only "trigger"; resolve / ack add when the audit slice
// can correlate "this event resolves that incident."
type Sink struct {
	name        string
	routingKey  string
	source      string
	clientName  string
	clientURL   string
	minSeverity output.Severity

	httpClient *http.Client
	mu         sync.Mutex
	closed     bool
}

// NewFromSpec is the SinkBuilder.
func NewFromSpec(spec output.SinkSpec) (output.Sink, error) {
	routingKey, err := output.SinkConfigString(spec.Config, "routing_key")
	if err != nil {
		return nil, err
	}
	if routingKey == "" {
		return nil, errors.New("pagerduty: config.routing_key is required")
	}
	// PagerDuty's Events API is a SaaS endpoint by design.  In
	// air-gap mode we refuse construction because the perimeter
	// can't reach events.pagerduty.com.  Tests inject a custom
	// URL via OverrideEventsAPIv2URL — we honour that path to
	// keep the tests runnable under air-gap.
	if err := airgap.Default().EndpointAllowed(apiURL); err != nil {
		return nil, fmt.Errorf("pagerduty: %w", err)
	}
	source, err := output.SinkConfigStringDefault(spec.Config, "source", "pg_hardstorage")
	if err != nil {
		return nil, err
	}
	clientName, err := output.SinkConfigStringDefault(spec.Config, "client", "pg_hardstorage")
	if err != nil {
		return nil, err
	}
	clientURL, err := output.SinkConfigString(spec.Config, "client_url")
	if err != nil {
		return nil, err
	}
	minSevStr, err := output.SinkConfigStringDefault(spec.Config, "min_severity", "error")
	if err != nil {
		return nil, err
	}
	minSev, perr := output.ParseSeverity(minSevStr)
	if perr != nil {
		return nil, fmt.Errorf("pagerduty: %w", perr)
	}
	return &Sink{
		name:        spec.Name,
		routingKey:  routingKey,
		source:      source,
		clientName:  clientName,
		clientURL:   clientURL,
		minSeverity: minSev,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

// Name implements output.Sink.
func (s *Sink) Name() string { return s.name }

// Open implements output.Sink. No-op (each Emit owns its request).
func (s *Sink) Open(_ context.Context, _ map[string]any) error { return nil }

// Emit implements output.Sink.
func (s *Sink) Emit(ctx context.Context, ev *output.Event) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("pagerduty: sink closed")
	}
	s.mu.Unlock()

	if !ev.Severity.AtLeast(s.minSeverity) {
		return nil
	}
	// Pre-Emit ctx check (consistent with syslog / email / opsgenie /
	// slack / webhook). Already-cancelled ctx bails before opening
	// the TCP connection to PD's Events API endpoint.
	if err := ctx.Err(); err != nil {
		return err
	}

	payload := buildPayload(ev, s.routingKey, s.source, s.clientName, s.clientURL)
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("pagerduty: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("pagerduty: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.pagerduty+json;version=2")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("pagerduty: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("pagerduty: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// Close implements output.Sink.
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// pdPayload is the PagerDuty Events-API-v2 enqueue body. Only the
// fields we populate are declared; extra fields PD adds at the
// service layer (escalation, integration metadata) come back via the
// API response we don't decode.
type pdPayload struct {
	RoutingKey  string   `json:"routing_key"`
	EventAction string   `json:"event_action"`
	DedupKey    string   `json:"dedup_key,omitempty"`
	Client      string   `json:"client,omitempty"`
	ClientURL   string   `json:"client_url,omitempty"`
	Payload     pdInner  `json:"payload"`
	Links       []pdLink `json:"links,omitempty"`
}

type pdInner struct {
	Summary       string         `json:"summary"`
	Source        string         `json:"source"`
	Severity      string         `json:"severity"`
	Component     string         `json:"component,omitempty"`
	Group         string         `json:"group,omitempty"`
	Class         string         `json:"class,omitempty"`
	CustomDetails map[string]any `json:"custom_details,omitempty"`
}

type pdLink struct {
	Href string `json:"href"`
	Text string `json:"text"`
}

func buildPayload(ev *output.Event, routingKey, source, clientName, clientURL string) pdPayload {
	out := pdPayload{
		RoutingKey:  routingKey,
		EventAction: "trigger",
		DedupKey:    dedupKeyFor(ev),
		Client:      clientName,
		ClientURL:   clientURL,
		Payload: pdInner{
			Summary:       summaryFor(ev),
			Source:        source,
			Severity:      mapSeverity(ev.Severity),
			Component:     ev.Component,
			Group:         ev.Subject.Deployment,
			Class:         ev.Op,
			CustomDetails: customDetailsFor(ev),
		},
	}
	if ev.Suggestion != nil && ev.Suggestion.DocURL != "" {
		out.Links = append(out.Links, pdLink{
			Href: ev.Suggestion.DocURL,
			Text: "runbook",
		})
	}
	return out
}

// summaryFor produces the short human-readable headline PagerDuty
// shows in the incident list. <=1024 chars per PD docs; we stay
// conservative.
func summaryFor(ev *output.Event) string {
	parts := []string{strings.ToUpper(ev.SeverityName), ev.Op}
	if ev.Subject.Deployment != "" {
		parts = append(parts, "deployment="+ev.Subject.Deployment)
	}
	if ev.Subject.BackupID != "" {
		parts = append(parts, "backup="+ev.Subject.BackupID)
	}
	s := strings.Join(parts, " · ")
	if len(s) > 1024 {
		s = s[:1024]
	}
	return s
}

// dedupKeyFor produces a deterministic dedup_key from the event's
// identity tuple. Same logical failure → same dedup_key → one PD
// incident. We hash so the key fits PD's 255-char limit and so
// case/format changes upstream don't collide unexpectedly.
func dedupKeyFor(ev *output.Event) string {
	tuple := strings.Join([]string{
		ev.Component, ev.Op,
		ev.Subject.Deployment, ev.Subject.BackupID,
	}, "\x00")
	sum := sha256.Sum256([]byte(tuple))
	return "pgh-" + hex.EncodeToString(sum[:])[:32]
}

// mapSeverity collapses our 8-level RFC 5424 ladder into PD's 4
// severities. Conservative: error stays error, anything more severe
// (alert / critical / emergency) becomes "critical."
func mapSeverity(s output.Severity) string {
	switch {
	case s <= output.SeverityCritical:
		return "critical"
	case s == output.SeverityError:
		return "error"
	case s == output.SeverityWarning:
		return "warning"
	default:
		return "info"
	}
}

// customDetailsFor packs the event's body + suggestion into PD's
// custom_details map. PD renders this as a key/value table on the
// incident detail page, which is what an on-call engineer sees first.
func customDetailsFor(ev *output.Event) map[string]any {
	out := map[string]any{}
	if ev.Subject.Tenant != "" {
		out["tenant"] = ev.Subject.Tenant
	}
	if ev.Subject.Timeline != 0 {
		out["timeline"] = ev.Subject.Timeline
	}
	if ev.Subject.LSN != "" {
		out["lsn"] = ev.Subject.LSN
	}
	if ev.Body != nil {
		out["body"] = ev.Body
	}
	if ev.Suggestion != nil {
		if ev.Suggestion.Human != "" {
			out["suggestion"] = ev.Suggestion.Human
		}
		if ev.Suggestion.Command != "" {
			out["suggested_command"] = ev.Suggestion.Command
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
