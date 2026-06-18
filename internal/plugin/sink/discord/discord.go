// Package discord implements an output.Sink that POSTs each Event
// to a Discord webhook URL using the embeds payload shape.
//
// Configuration (YAML keys):
//
//	plugin: discord
//	config:
//	  webhook_url: https://discord.com/api/webhooks/.../...   # required
//	  username: pg_hardstorage                                # default
//	  avatar_url: ""                                          # optional
//	  min_severity: warning                                   # default
//	  timeout: 10s                                            # default
//
// Discord's webhook accepts up to 10 embeds per request; we send
// one embed per Event with title/description/fields/timestamp/
// color filled in.  Color is the integer encoding Discord uses
// for embed-stripe color.
package discord

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
	output.DefaultSinkRegistry.Register("discord", NewFromSpec)
}

// Sink emits to a Discord webhook.
type Sink struct {
	name        string
	webhookURL  string
	username    string
	avatarURL   string
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
		return nil, errors.New("discord: config.webhook_url is required")
	}
	if err := airgap.Default().EndpointAllowed(url); err != nil {
		return nil, fmt.Errorf("discord: %w", err)
	}

	username, _ := output.SinkConfigStringDefault(spec.Config, "username", "pg_hardstorage")
	avatarURL, _ := output.SinkConfigString(spec.Config, "avatar_url")

	minSevStr, err := output.SinkConfigStringDefault(spec.Config, "min_severity", "warning")
	if err != nil {
		return nil, err
	}
	minSev, perr := output.ParseSeverity(minSevStr)
	if perr != nil {
		return nil, fmt.Errorf("discord: %w", perr)
	}

	timeoutStr, _ := output.SinkConfigStringDefault(spec.Config, "timeout", "10s")
	timeout, perr := time.ParseDuration(timeoutStr)
	if perr != nil {
		return nil, fmt.Errorf("discord: parse timeout: %w", perr)
	}

	return &Sink{
		name:        spec.Name,
		webhookURL:  url,
		username:    username,
		avatarURL:   avatarURL,
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
		return errors.New("discord: sink closed")
	}
	s.mu.Unlock()
	if !ev.Severity.AtLeast(s.minSeverity) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	payload := buildPayload(ev, s.username, s.avatarURL)
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("discord: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("discord: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("discord: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("discord: status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
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

// buildPayload constructs Discord's webhook execute body with one
// embed per event.
func buildPayload(ev *output.Event, username, avatarURL string) map[string]any {
	fields := []map[string]any{}
	addField := func(name, value string) {
		if value == "" {
			return
		}
		fields = append(fields, map[string]any{"name": name, "value": value, "inline": true})
	}
	addField("Severity", ev.Severity.String())
	addField("Component", ev.Component)
	addField("Op", ev.Op)
	addField("Tenant", ev.Subject.Tenant)
	addField("Deployment", ev.Subject.Deployment)
	addField("Backup", ev.Subject.BackupID)

	embed := map[string]any{
		"title":     titleFor(ev),
		"color":     colorInt(ev.Severity),
		"timestamp": ev.GeneratedAt.UTC().Format(time.RFC3339),
		"fields":    fields,
	}
	if ev.Suggestion != nil && ev.Suggestion.Human != "" {
		embed["description"] = "**Suggestion:** " + ev.Suggestion.Human
	}

	out := map[string]any{
		"username": username,
		"embeds":   []map[string]any{embed},
	}
	if avatarURL != "" {
		out["avatar_url"] = avatarURL
	}
	return out
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

// colorInt returns the integer encoding Discord uses for
// embed-stripe color.  We pick a small palette: red for
// error/critical, orange for warning, blue for notice, grey for
// info/debug.
func colorInt(s output.Severity) int {
	switch s {
	case output.SeverityEmergency, output.SeverityAlert, output.SeverityCritical, output.SeverityError:
		return 0xE74C3C // red
	case output.SeverityWarning:
		return 0xE67E22 // orange
	case output.SeverityNotice:
		return 0x3498DB // blue
	case output.SeverityInfo, output.SeverityDebug:
		return 0x95A5A6 // grey
	}
	return 0
}
