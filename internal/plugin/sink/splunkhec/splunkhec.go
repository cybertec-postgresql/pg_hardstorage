// Package splunkhec implements an output.Sink that POSTs each
// Event to a Splunk HTTP Event Collector (HEC) endpoint.
//
// Configuration (YAML keys):
//
//	plugin: splunk-hec
//	config:
//	  url: https://splunk.example.com:8088/services/collector/event
//	  token: <hec-token>                        # required
//	  index: pg_hardstorage                     # optional
//	  source: pg_hardstorage                    # optional
//	  sourcetype: _json                         # default
//	  host: ""                                  # default: os.Hostname()
//	  min_severity: notice                      # default
//	  timeout: 10s                              # default
//	  insecure_skip_verify: false               # for self-signed dev clusters
//
// Splunk HEC accepts JSON payloads of the form:
//
//	{"time":"<epoch>","host":"...","source":"...","sourcetype":"...",
//	 "index":"...","event":{<arbitrary json>}}
//
// We send one event per request (no batching) — that matches the
// rest of our sink stack and avoids the buffering questions of a
// HEC-batched mode.  Operators with high event volume put a
// HEC-aware aggregator in front (filebeat / fluentbit), the
// same pattern Splunk recommends.
package splunkhec

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func init() {
	output.DefaultSinkRegistry.Register("splunk-hec", NewFromSpec)
}

// Sink emits events to a Splunk HEC endpoint.
type Sink struct {
	name        string
	url         string
	token       string
	index       string
	source      string
	sourcetype  string
	host        string
	minSeverity output.Severity

	httpClient *http.Client
	mu         sync.Mutex
	closed     bool
}

// NewFromSpec builds a Splunk HEC sink from a SinkSpec.
func NewFromSpec(spec output.SinkSpec) (output.Sink, error) {
	url, err := output.SinkConfigString(spec.Config, "url")
	if err != nil {
		return nil, err
	}
	if url == "" {
		return nil, errors.New("splunk-hec: config.url is required")
	}
	if err := airgap.Default().EndpointAllowed(url); err != nil {
		return nil, fmt.Errorf("splunk-hec: %w", err)
	}
	token, err := output.SinkConfigString(spec.Config, "token")
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("splunk-hec: config.token is required")
	}

	index, _ := output.SinkConfigString(spec.Config, "index")
	source, _ := output.SinkConfigString(spec.Config, "source")
	sourcetype, _ := output.SinkConfigStringDefault(spec.Config, "sourcetype", "_json")
	host, _ := output.SinkConfigString(spec.Config, "host")
	if host == "" {
		host, _ = os.Hostname()
	}

	minSevStr, err := output.SinkConfigStringDefault(spec.Config, "min_severity", "notice")
	if err != nil {
		return nil, err
	}
	minSev, perr := output.ParseSeverity(minSevStr)
	if perr != nil {
		return nil, fmt.Errorf("splunk-hec: %w", perr)
	}

	timeoutStr, _ := output.SinkConfigStringDefault(spec.Config, "timeout", "10s")
	timeout, perr := time.ParseDuration(timeoutStr)
	if perr != nil {
		return nil, fmt.Errorf("splunk-hec: parse timeout: %w", perr)
	}

	insecureSkip := false
	if v, ok := spec.Config["insecure_skip_verify"].(bool); ok {
		insecureSkip = v
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	if insecureSkip {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	return &Sink{
		name:        spec.Name,
		url:         url,
		token:       token,
		index:       index,
		source:      source,
		sourcetype:  sourcetype,
		host:        host,
		minSeverity: minSev,
		httpClient:  &http.Client{Timeout: timeout, Transport: tr},
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
		return errors.New("splunk-hec: sink closed")
	}
	s.mu.Unlock()
	if !ev.Severity.AtLeast(s.minSeverity) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	body := map[string]any{
		"time":       float64(ev.GeneratedAt.UnixNano()) / 1e9,
		"host":       s.host,
		"sourcetype": s.sourcetype,
		"event":      ev,
	}
	if s.index != "" {
		body["index"] = s.index
	}
	if s.source != "" {
		body["source"] = s.source
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("splunk-hec: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("splunk-hec: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Splunk "+s.token)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("splunk-hec: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("splunk-hec: status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
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
