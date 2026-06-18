// Package otelevents implements an output.Sink that POSTs each
// Event to an OpenTelemetry collector via OTLP/HTTP+JSON, encoded
// as the OTLP `LogRecord` shape.
//
// Configuration (YAML keys):
//
//	plugin: otel-events
//	config:
//	  endpoint: http://otel-collector:4318       # required (OTLP/HTTP root)
//	  service_name: pg_hardstorage               # default
//	  headers:                                   # optional auth / routing
//	    x-honeycomb-team: <key>
//	  min_severity: notice                       # default
//	  timeout: 10s                               # default
//
// Why OTLP/HTTP+JSON and not the protobuf encoding?  JSON is in the
// OTLP spec (https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding),
// every reference collector accepts it, and using JSON keeps the
// sink dependency-free — we don't have to pull in the OTel logs
// protobuf SDK just to emit one logs payload per event.
//
// Events become OTLP LogRecords with:
//
//   - severity_number: RFC 5424 → OTel mapping
//   - severity_text:   our severity name
//   - body.kvlist_value: the Event JSON flattened
//   - attributes:      schema, component, op, subject.* (one per nonempty)
package otelevents

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
	output.DefaultSinkRegistry.Register("otel-events", NewFromSpec)
}

// Sink emits OTLP/HTTP+JSON logs.
type Sink struct {
	name        string
	endpoint    string
	logsURL     string
	serviceName string
	headers     map[string]string
	minSeverity output.Severity

	httpClient *http.Client
	mu         sync.Mutex
	closed     bool
}

// NewFromSpec builds the sink.
func NewFromSpec(spec output.SinkSpec) (output.Sink, error) {
	endpoint, err := output.SinkConfigString(spec.Config, "endpoint")
	if err != nil {
		return nil, err
	}
	if endpoint == "" {
		return nil, errors.New("otel-events: config.endpoint is required (OTLP/HTTP collector root URL)")
	}
	if err := airgap.Default().EndpointAllowed(endpoint); err != nil {
		return nil, fmt.Errorf("otel-events: %w", err)
	}
	logsURL := strings.TrimRight(endpoint, "/") + "/v1/logs"

	serviceName, _ := output.SinkConfigStringDefault(spec.Config, "service_name", "pg_hardstorage")

	headers := map[string]string{}
	if v, ok := spec.Config["headers"]; ok {
		switch x := v.(type) {
		case map[string]any:
			for k, vv := range x {
				if s, ok := vv.(string); ok {
					headers[k] = s
				}
			}
		case map[string]string:
			for k, vv := range x {
				headers[k] = vv
			}
		case nil:
			// absent
		default:
			return nil, fmt.Errorf("otel-events: config.headers must be a map (got %T)", v)
		}
	}

	minSevStr, err := output.SinkConfigStringDefault(spec.Config, "min_severity", "notice")
	if err != nil {
		return nil, err
	}
	minSev, perr := output.ParseSeverity(minSevStr)
	if perr != nil {
		return nil, fmt.Errorf("otel-events: %w", perr)
	}

	timeoutStr, _ := output.SinkConfigStringDefault(spec.Config, "timeout", "10s")
	timeout, perr := time.ParseDuration(timeoutStr)
	if perr != nil {
		return nil, fmt.Errorf("otel-events: parse timeout: %w", perr)
	}

	return &Sink{
		name:        spec.Name,
		endpoint:    endpoint,
		logsURL:     logsURL,
		serviceName: serviceName,
		headers:     headers,
		minSeverity: minSev,
		httpClient:  &http.Client{Timeout: timeout},
	}, nil
}

// Name implements output.Sink.
func (s *Sink) Name() string { return s.name }

// Open implements output.Sink. No-op.
func (s *Sink) Open(_ context.Context, _ map[string]any) error { return nil }

// Emit implements output.Sink.  Builds one OTLP/HTTP+JSON ResourceLogs
// payload per event.  Batching is intentionally not done here:
// operators with high event volume put a sidecar collector in front
// of the upstream backend.
func (s *Sink) Emit(ctx context.Context, ev *output.Event) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("otel-events: sink closed")
	}
	s.mu.Unlock()
	if !ev.Severity.AtLeast(s.minSeverity) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	payload := buildLogsPayload(ev, s.serviceName)
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("otel-events: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.logsURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("otel-events: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range s.headers {
		req.Header.Set(k, v)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("otel-events: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("otel-events: status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
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

// buildLogsPayload renders one Event as the OTLP/HTTP+JSON
// `resourceLogs` shape.  We populate the minimum a collector
// needs: resource.attributes (service.name), scope (this
// package's name), one logRecord per event.
func buildLogsPayload(ev *output.Event, serviceName string) map[string]any {
	attrs := []map[string]any{}
	addAttr := func(key, value string) {
		if value == "" {
			return
		}
		attrs = append(attrs, map[string]any{
			"key":   key,
			"value": map[string]any{"stringValue": value},
		})
	}
	addAttr("schema", ev.Schema)
	addAttr("component", ev.Component)
	addAttr("op", ev.Op)
	addAttr("tenant", ev.Subject.Tenant)
	addAttr("deployment", ev.Subject.Deployment)
	addAttr("backup_id", ev.Subject.BackupID)

	bodyJSON, _ := json.Marshal(ev)

	logRecord := map[string]any{
		"timeUnixNano":   ev.GeneratedAt.UnixNano(),
		"severityNumber": mapSeverityNumber(ev.Severity),
		"severityText":   ev.Severity.String(),
		"body":           map[string]any{"stringValue": string(bodyJSON)},
		"attributes":     attrs,
	}

	return map[string]any{
		"resourceLogs": []map[string]any{
			{
				"resource": map[string]any{
					"attributes": []map[string]any{
						{"key": "service.name", "value": map[string]any{"stringValue": serviceName}},
					},
				},
				"scopeLogs": []map[string]any{
					{
						"scope":      map[string]any{"name": "github.com/cybertec-postgresql/pg_hardstorage"},
						"logRecords": []map[string]any{logRecord},
					},
				},
			},
		},
	}
}

// mapSeverityNumber maps RFC 5424 severities to OTel severity
// numbers per the spec
// (https://opentelemetry.io/docs/specs/otel/logs/data-model/#severity-fields).
func mapSeverityNumber(s output.Severity) int {
	switch s {
	case output.SeverityEmergency:
		return 24 // FATAL4
	case output.SeverityAlert:
		return 23 // FATAL3
	case output.SeverityCritical:
		return 22 // FATAL2
	case output.SeverityError:
		return 17 // ERROR
	case output.SeverityWarning:
		return 13 // WARN
	case output.SeverityNotice:
		return 10 // INFO2
	case output.SeverityInfo:
		return 9 // INFO
	case output.SeverityDebug:
		return 5 // DEBUG
	}
	return 0
}
