// Package slack implements an output.Sink that posts events to a
// Slack incoming webhook.
//
// Configuration (YAML keys):
//
//	plugin: slack
//	config:
//	  webhook_url: https://hooks.slack.com/services/T.../B.../...
//	  channel: "#pg-backups"            # optional, overrides the webhook's default
//	  username: "pg_hardstorage"        # optional bot username
//	  min_severity: warning             # optional; default: notice
//
// The webhook URL is treated as a secret. Operators wanting the URL
// indirected through KMS or a system keyring use the future
// `kms-secret://` indirection (a feature); v0.1 takes the URL
// inline.
//
// Why a focused, declarative config rather than a "one big sink with
// every Slack feature"? Because every sink in our model has the same
// shape (Open/Emit/Close + a config bag), and Slack-specific niceties
// (blocks, attachments, threads) can be layered on top via per-sink
// config keys without changing the contract.
package slack

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
	output.DefaultSinkRegistry.Register("slack", NewFromSpec)
}

// Sink posts events to a Slack incoming webhook.
type Sink struct {
	name        string
	webhookURL  string
	channel     string
	username    string
	minSeverity output.Severity

	httpClient *http.Client
	mu         sync.Mutex
	closed     bool
}

// NewFromSpec is the SinkBuilder. Validates required keys and returns
// a ready-to-Open Sink. The dispatcher calls Open before the first
// Emit; Open is currently a no-op for Slack but kept for future
// auth-handshake and timeout-tuning needs.
func NewFromSpec(spec output.SinkSpec) (output.Sink, error) {
	url, err := output.SinkConfigString(spec.Config, "webhook_url")
	if err != nil {
		return nil, err
	}
	if url == "" {
		return nil, errors.New("slack: config.webhook_url is required")
	}
	if err := airgap.Default().EndpointAllowed(url); err != nil {
		return nil, fmt.Errorf("slack: %w", err)
	}
	channel, err := output.SinkConfigString(spec.Config, "channel")
	if err != nil {
		return nil, err
	}
	username, err := output.SinkConfigStringDefault(spec.Config, "username", "pg_hardstorage")
	if err != nil {
		return nil, err
	}
	minSevStr, err := output.SinkConfigStringDefault(spec.Config, "min_severity", "notice")
	if err != nil {
		return nil, err
	}
	minSev, perr := output.ParseSeverity(minSevStr)
	if perr != nil {
		return nil, fmt.Errorf("slack: %w", perr)
	}

	return &Sink{
		name:        spec.Name,
		webhookURL:  url,
		channel:     channel,
		username:    username,
		minSeverity: minSev,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

// Name implements output.Sink.
func (s *Sink) Name() string { return s.name }

// Open implements output.Sink. No-op today; reserved for future
// resource setup (Slack does not require an explicit handshake).
func (s *Sink) Open(_ context.Context, _ map[string]any) error {
	return nil
}

// Emit implements output.Sink. Drops events below min_severity.
// Errors are returned to the caller (the dispatcher) but never
// propagate to the foreground command — a flaky Slack must not
// break a backup.
func (s *Sink) Emit(ctx context.Context, ev *output.Event) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("slack: sink closed")
	}
	s.mu.Unlock()

	if !ev.Severity.AtLeast(s.minSeverity) {
		// AtLeast handles the "lower number = more severe" RFC 5424
		// inversion so the comparison reads naturally: "drop unless
		// the event is at least as severe as the threshold." Same
		// idiom as the webhook and syslog sinks; consistency lets
		// reviewers triage all three at once.
		return nil
	}
	// Pre-Emit ctx check (consistent with syslog / email / opsgenie /
	// pagerduty / webhook). A cancelled ctx must bail BEFORE we
	// open a TCP connection. http.NewRequestWithContext +
	// httpClient.Do honour ctx through the request lifecycle, but an
	// already-cancelled ctx is cheaper to surface here than to reach
	// connection establishment.
	if err := ctx.Err(); err != nil {
		return err
	}

	payload := buildPayload(ev, s.channel, s.username)
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("slack: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("slack: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("slack: post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		// Slack returns "ok" / "no_text" / etc. Capture the body so
		// the failure event has the real error message.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("slack: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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

// payload is what Slack's incoming-webhooks API consumes. Top-level
// "text" is the fallback; "blocks" carries structure for clients that
// support Block Kit.
type payload struct {
	Channel  string  `json:"channel,omitempty"`
	Username string  `json:"username,omitempty"`
	Text     string  `json:"text"`
	Blocks   []block `json:"blocks,omitempty"`
}

type block struct {
	Type string `json:"type"`
	Text *text  `json:"text,omitempty"`
}

type text struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func buildPayload(ev *output.Event, channel, username string) payload {
	header := fmt.Sprintf("%s *%s* — `%s`",
		severityIcon(ev.Severity),
		strings.ToUpper(ev.SeverityName),
		ev.Op)

	subject := renderSubject(ev.Subject)
	if subject != "" {
		header += "  · " + subject
	}

	body := renderBody(ev)

	plainText := header
	if body != "" {
		plainText += "\n" + body
	}

	blocks := []block{
		{Type: "section", Text: &text{Type: "mrkdwn", Text: header}},
	}
	if body != "" {
		blocks = append(blocks, block{Type: "section", Text: &text{Type: "mrkdwn", Text: body}})
	}
	if ev.Suggestion != nil && ev.Suggestion.Human != "" {
		s := "💡 " + ev.Suggestion.Human
		if ev.Suggestion.Command != "" {
			s += "\n```\n" + ev.Suggestion.Command + "\n```"
		}
		blocks = append(blocks, block{Type: "section", Text: &text{Type: "mrkdwn", Text: s}})
	}

	return payload{
		Channel:  channel,
		Username: username,
		Text:     plainText,
		Blocks:   blocks,
	}
}

// severityIcon picks an emoji for the severity, mirroring the kind of
// glanceable signal the rest of the operator UI gives.
func severityIcon(s output.Severity) string {
	switch {
	case s <= output.SeverityCritical:
		return "🚨"
	case s == output.SeverityError:
		return "❌"
	case s == output.SeverityWarning:
		return "⚠️"
	case s == output.SeverityNotice:
		return "ℹ️"
	default:
		return "📋"
	}
}

func renderSubject(s output.Subject) string {
	parts := []string{}
	if s.Deployment != "" {
		parts = append(parts, "deployment="+s.Deployment)
	}
	if s.Tenant != "" && s.Tenant != "default" {
		parts = append(parts, "tenant="+s.Tenant)
	}
	if s.BackupID != "" {
		parts = append(parts, "backup="+s.BackupID)
	}
	if s.Timeline != 0 {
		parts = append(parts, fmt.Sprintf("tli=%d", s.Timeline))
	}
	if s.LSN != "" {
		parts = append(parts, "lsn="+s.LSN)
	}
	return strings.Join(parts, " · ")
}

func renderBody(ev *output.Event) string {
	if ev.Body == nil {
		return ""
	}
	// Try a structured render — JSON-encoding any map / struct gives
	// a compact, readable view inside Slack's monospaced code block.
	js, err := json.MarshalIndent(ev.Body, "", "  ")
	if err != nil {
		// Fall back to %v — never fail to emit because of a marshal hiccup.
		return fmt.Sprintf("```\n%v\n```", ev.Body)
	}
	return "```\n" + string(js) + "\n```"
}
