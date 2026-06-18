// Package teams implements an output.Sink that POSTs each Event
// to a Microsoft Teams Incoming Webhook URL using the AdaptiveCard
// payload format.
//
// Configuration (YAML keys):
//
//	plugin: teams
//	config:
//	  webhook_url: https://outlook.office.com/webhook/...   # required
//	  min_severity: warning                                 # default
//	  timeout: 10s                                          # default
//
// Teams webhooks accept the legacy MessageCard format and the newer
// AdaptiveCard v1.5 format wrapped in an `attachments` envelope.
// We emit AdaptiveCard because Microsoft has been steering the
// platform there since late 2024 — MessageCards still work but are
// declared "legacy" in the docs.  AdaptiveCard renders cleanly in
// both desktop and mobile Teams clients.
package teams

import (
	"bytes"
	"context"
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
	output.DefaultSinkRegistry.Register("teams", NewFromSpec)
}

// Sink emits to a Teams Incoming Webhook.
type Sink struct {
	name        string
	webhookURL  string
	minSeverity output.Severity

	httpClient *http.Client
	mu         sync.Mutex
	closed     bool
}

// NewFromSpec builds the sink.
func NewFromSpec(spec output.SinkSpec) (output.Sink, error) {
	url, err := output.SinkConfigString(spec.Config, "webhook_url")
	if err != nil {
		return nil, err
	}
	if url == "" {
		return nil, errors.New("teams: config.webhook_url is required")
	}
	if err := airgap.Default().EndpointAllowed(url); err != nil {
		return nil, fmt.Errorf("teams: %w", err)
	}

	minSevStr, err := output.SinkConfigStringDefault(spec.Config, "min_severity", "warning")
	if err != nil {
		return nil, err
	}
	minSev, perr := output.ParseSeverity(minSevStr)
	if perr != nil {
		return nil, fmt.Errorf("teams: %w", perr)
	}

	timeoutStr, _ := output.SinkConfigStringDefault(spec.Config, "timeout", "10s")
	timeout, perr := time.ParseDuration(timeoutStr)
	if perr != nil {
		return nil, fmt.Errorf("teams: parse timeout: %w", perr)
	}

	return &Sink{
		name:        spec.Name,
		webhookURL:  url,
		minSeverity: minSev,
		httpClient:  &http.Client{Timeout: timeout},
	}, nil
}

// Name implements output.Sink.
func (s *Sink) Name() string { return s.name }

// Open implements output.Sink. No-op.
func (s *Sink) Open(_ context.Context, _ map[string]any) error { return nil }

// Emit implements output.Sink.
func (s *Sink) Emit(ctx context.Context, ev *output.Event) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("teams: sink closed")
	}
	s.mu.Unlock()
	if !ev.Severity.AtLeast(s.minSeverity) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	payload := buildAdaptiveCard(ev)
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("teams: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("teams: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("teams: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("teams: status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
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

// buildAdaptiveCard wraps the Event into an AdaptiveCard payload.
// The outer envelope is the standard "type=message + attachments"
// shape Teams expects; the card itself uses AdaptiveCard 1.5.
func buildAdaptiveCard(ev *output.Event) map[string]any {
	color := colorFor(ev.Severity)

	facts := []map[string]any{}
	addFact := func(title, value string) {
		if value == "" {
			return
		}
		facts = append(facts, map[string]any{"title": title, "value": value})
	}
	addFact("Severity", ev.Severity.String())
	addFact("Component", ev.Component)
	addFact("Op", ev.Op)
	addFact("Tenant", ev.Subject.Tenant)
	addFact("Deployment", ev.Subject.Deployment)
	addFact("BackupID", ev.Subject.BackupID)

	body := []map[string]any{
		{
			"type":   "TextBlock",
			"text":   titleFor(ev),
			"size":   "Medium",
			"weight": "Bolder",
			"color":  color,
			"wrap":   true,
		},
	}
	if len(facts) > 0 {
		body = append(body, map[string]any{
			"type":  "FactSet",
			"facts": facts,
		})
	}
	if ev.Suggestion != nil && ev.Suggestion.Human != "" {
		body = append(body, map[string]any{
			"type": "TextBlock",
			"text": "**Suggestion:** " + ev.Suggestion.Human,
			"wrap": true,
		})
	}

	card := map[string]any{
		"type":    "AdaptiveCard",
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"version": "1.5",
		"body":    body,
	}
	return map[string]any{
		"type": "message",
		"attachments": []map[string]any{
			{
				"contentType": "application/vnd.microsoft.card.adaptive",
				"content":     card,
			},
		},
	}
}

func titleFor(ev *output.Event) string {
	parts := []string{}
	if ev.Component != "" {
		parts = append(parts, ev.Component)
	}
	if ev.Op != "" {
		parts = append(parts, ev.Op)
	}
	t := strings.Join(parts, ": ")
	if t == "" {
		t = "pg_hardstorage event"
	}
	return t
}

// colorFor maps severity to AdaptiveCard's TextBlock color
// vocabulary.  attention=red, warning=orange, accent=blue,
// good=green, default=neutral.
func colorFor(s output.Severity) string {
	switch s {
	case output.SeverityEmergency, output.SeverityAlert, output.SeverityCritical, output.SeverityError:
		return "attention"
	case output.SeverityWarning:
		return "warning"
	case output.SeverityNotice, output.SeverityInfo:
		return "default"
	case output.SeverityDebug:
		return "default"
	}
	return "default"
}
