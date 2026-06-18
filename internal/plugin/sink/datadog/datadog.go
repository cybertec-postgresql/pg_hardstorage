// Package datadog implements an output.Sink that POSTs each Event
// to Datadog's Events API (v1).
//
// Configuration (YAML keys):
//
//	plugin: datadog-events
//	config:
//	  api_key: <dd-api-key>                     # required
//	  site: datadoghq.com                       # default; eu = datadoghq.eu, gov = ddog-gov.com
//	  source_type_name: pg_hardstorage          # appears as "Source" in DD UI
//	  tags: ["env:prod", "tier:pg"]             # default tags applied to every event
//	  min_severity: notice                      # default
//	  timeout: 10s                              # default
//
// Datadog Events API:
//
//	POST https://api.<site>/api/v1/events
//	headers: DD-API-KEY: <key>
//	body: {title, text, alert_type, source_type_name, tags, date_happened}
//
// alert_type maps RFC 5424 → Datadog (info, success, warning, error,
// user_update). We use info / warning / error; emergency..critical
// collapses to error.  The full Event JSON is inlined into the
// `text` field as a fenced code block so the Datadog UI shows it
// without losing structure.
package datadog

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
	output.DefaultSinkRegistry.Register("datadog-events", NewFromSpec)
}

// Sink emits to Datadog Events API.
type Sink struct {
	name           string
	apiURL         string
	apiKey         string
	sourceTypeName string
	defaultTags    []string
	minSeverity    output.Severity

	httpClient *http.Client
	mu         sync.Mutex
	closed     bool
}

// NewFromSpec builds the sink.
func NewFromSpec(spec output.SinkSpec) (output.Sink, error) {
	apiKey, err := output.SinkConfigString(spec.Config, "api_key")
	if err != nil {
		return nil, err
	}
	if apiKey == "" {
		return nil, errors.New("datadog-events: config.api_key is required")
	}
	site, _ := output.SinkConfigStringDefault(spec.Config, "site", "datadoghq.com")
	apiURL := "https://api." + site + "/api/v1/events"
	if err := airgap.Default().EndpointAllowed(apiURL); err != nil {
		return nil, fmt.Errorf("datadog-events: %w", err)
	}

	sourceTypeName, _ := output.SinkConfigStringDefault(spec.Config, "source_type_name", "pg_hardstorage")

	var defaultTags []string
	if v, ok := spec.Config["tags"]; ok {
		switch x := v.(type) {
		case []any:
			for _, item := range x {
				if s, ok := item.(string); ok {
					defaultTags = append(defaultTags, s)
				}
			}
		case []string:
			defaultTags = append(defaultTags, x...)
		case nil:
			// absent
		default:
			return nil, fmt.Errorf("datadog-events: config.tags must be a list of strings (got %T)", v)
		}
	}

	minSevStr, err := output.SinkConfigStringDefault(spec.Config, "min_severity", "notice")
	if err != nil {
		return nil, err
	}
	minSev, perr := output.ParseSeverity(minSevStr)
	if perr != nil {
		return nil, fmt.Errorf("datadog-events: %w", perr)
	}

	timeoutStr, _ := output.SinkConfigStringDefault(spec.Config, "timeout", "10s")
	timeout, perr := time.ParseDuration(timeoutStr)
	if perr != nil {
		return nil, fmt.Errorf("datadog-events: parse timeout: %w", perr)
	}

	return &Sink{
		name:           spec.Name,
		apiURL:         apiURL,
		apiKey:         apiKey,
		sourceTypeName: sourceTypeName,
		defaultTags:    defaultTags,
		minSeverity:    minSev,
		httpClient:     &http.Client{Timeout: timeout},
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
		return errors.New("datadog-events: sink closed")
	}
	s.mu.Unlock()
	if !ev.Severity.AtLeast(s.minSeverity) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	title := titleFor(ev)
	text := textFor(ev)
	alertType := mapAlertType(ev.Severity)

	tags := append([]string{}, s.defaultTags...)
	if ev.Subject.Tenant != "" {
		tags = append(tags, "tenant:"+ev.Subject.Tenant)
	}
	if ev.Subject.Deployment != "" {
		tags = append(tags, "deployment:"+ev.Subject.Deployment)
	}
	if ev.Component != "" {
		tags = append(tags, "component:"+ev.Component)
	}
	if ev.Op != "" {
		tags = append(tags, "op:"+ev.Op)
	}
	tags = append(tags, "severity:"+ev.Severity.String())

	body := map[string]any{
		"title":            title,
		"text":             text,
		"alert_type":       alertType,
		"source_type_name": s.sourceTypeName,
		"tags":             tags,
		"date_happened":    ev.GeneratedAt.Unix(),
	}
	if ev.Subject.Deployment != "" {
		body["aggregation_key"] = ev.Subject.Deployment
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("datadog-events: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("datadog-events: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("DD-API-KEY", s.apiKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("datadog-events: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("datadog-events: status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
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

func titleFor(ev *output.Event) string {
	parts := []string{}
	if ev.Component != "" {
		parts = append(parts, ev.Component)
	}
	if ev.Op != "" {
		parts = append(parts, ev.Op)
	}
	title := strings.Join(parts, ": ")
	if title == "" {
		title = "pg_hardstorage event"
	}
	if ev.Subject.Deployment != "" {
		title = "[" + ev.Subject.Deployment + "] " + title
	}
	return title
}

func textFor(ev *output.Event) string {
	body, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return fmt.Sprintf("(failed to render event JSON: %v)", err)
	}
	// Datadog supports markdown; the %%% wrapping turns a fenced
	// block into syntax-highlighted markdown.
	return "%%%\n```json\n" + string(body) + "\n```\n%%%"
}

// mapAlertType maps the RFC 5424 severity onto Datadog's
// alert_type enum.
func mapAlertType(s output.Severity) string {
	switch s {
	case output.SeverityEmergency, output.SeverityAlert, output.SeverityCritical, output.SeverityError:
		return "error"
	case output.SeverityWarning:
		return "warning"
	default:
		return "info"
	}
}
