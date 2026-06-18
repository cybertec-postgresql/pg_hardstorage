// Package webhook implements an output.Sink that POSTs each event as
// JSON to a configured URL.
//
// Configuration (YAML keys):
//
//	plugin: webhook
//	config:
//	  url: https://ops.example.com/pg-hardstorage
//	  method: POST                       # default POST; PUT also accepted
//	  auth_header: "Bearer eyJ..."       # optional; sent as Authorization
//	  content_type: application/json     # default; override only for niche endpoints
//	  min_severity: warning              # default: notice
//	  timeout: 10s                       # default
//
// The body is the same Event JSON the dispatcher renders (schema =
// pg_hardstorage.v1) — operators wanting a different shape can put a
// transformer in front. Same-shape-everywhere keeps the data plane
// boring on purpose.
package webhook

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
	output.DefaultSinkRegistry.Register("webhook", NewFromSpec)
}

// Sink POSTs JSON-encoded events to a fixed URL.
type Sink struct {
	name        string
	url         string
	method      string
	authHeader  string
	contentType string
	minSeverity output.Severity

	httpClient *http.Client
	mu         sync.Mutex
	closed     bool
}

// NewFromSpec is the SinkBuilder.
func NewFromSpec(spec output.SinkSpec) (output.Sink, error) {
	url, err := output.SinkConfigString(spec.Config, "url")
	if err != nil {
		return nil, err
	}
	if url == "" {
		return nil, errors.New("webhook: config.url is required")
	}
	if err := airgap.Default().EndpointAllowed(url); err != nil {
		return nil, fmt.Errorf("webhook: %w", err)
	}
	method, err := output.SinkConfigStringDefault(spec.Config, "method", http.MethodPost)
	if err != nil {
		return nil, err
	}
	method = strings.ToUpper(method)
	switch method {
	case http.MethodPost, http.MethodPut:
	default:
		return nil, fmt.Errorf("webhook: unsupported method %q (allowed: POST, PUT)", method)
	}

	authHeader, err := output.SinkConfigString(spec.Config, "auth_header")
	if err != nil {
		return nil, err
	}
	contentType, err := output.SinkConfigStringDefault(spec.Config, "content_type", "application/json")
	if err != nil {
		return nil, err
	}
	minSevStr, err := output.SinkConfigStringDefault(spec.Config, "min_severity", "notice")
	if err != nil {
		return nil, err
	}
	minSev, perr := output.ParseSeverity(minSevStr)
	if perr != nil {
		return nil, fmt.Errorf("webhook: %w", perr)
	}

	timeoutStr, err := output.SinkConfigStringDefault(spec.Config, "timeout", "10s")
	if err != nil {
		return nil, err
	}
	timeout, perr := time.ParseDuration(timeoutStr)
	if perr != nil {
		return nil, fmt.Errorf("webhook: parse timeout %q: %w", timeoutStr, perr)
	}

	return &Sink{
		name:        spec.Name,
		url:         url,
		method:      method,
		authHeader:  authHeader,
		contentType: contentType,
		minSeverity: minSev,
		httpClient:  &http.Client{Timeout: timeout},
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
		return errors.New("webhook: sink closed")
	}
	s.mu.Unlock()

	if !ev.Severity.AtLeast(s.minSeverity) {
		return nil
	}
	// Pre-Emit ctx check (consistent with syslog / email / opsgenie /
	// pagerduty / slack). Already-cancelled ctx bails before any
	// network work.
	if err := ctx.Err(); err != nil {
		return err
	}

	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("webhook: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, s.method, s.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook: build request: %w", err)
	}
	req.Header.Set("Content-Type", s.contentType)
	if s.authHeader != "" {
		req.Header.Set("Authorization", s.authHeader)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("webhook: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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
