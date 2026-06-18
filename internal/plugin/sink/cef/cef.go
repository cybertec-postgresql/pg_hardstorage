// Package cef implements an output.Sink emitting ArcSight Common
// Event Format (CEF) records.
//
// Configuration (YAML keys):
//
//	plugin: cef
//	config:
//	  destination: file:///var/log/pg_hardstorage/audit.cef   # default
//	  vendor: pg_hardstorage                                  # CEF Vendor (default)
//	  product: pg_hardstorage                                 # CEF Product (default)
//	  version: "1"                                            # CEF Version (default)
//	  min_severity: notice                                    # default
//
// CEF is what every legacy SIEM (ArcSight, QRadar, RSA, Splunk via
// CEF connector) speaks natively. Each event renders as a single
// line:
//
//	CEF:0|<vendor>|<product>|<version>|<signatureId>|<name>|<severity>|<extension>
//
// Where <severity> maps RFC 5424 → CEF (0..10) per ArcSight's
// recommendation:
//
//	emergency / alert / critical → 10  (Very-High)
//	error                       → 8   (High)
//	warning                     → 6   (Medium)
//	notice                      → 4   (Low)
//	info                        → 2
//	debug                       → 1
//
// We deliberately keep this neutral — the destination is a file
// (the common case for SIEM forwarders), not a TCP/UDP target.
// Operators who need network transport pipe the file through
// rsyslog or filebeat, the same way Splunk / ArcSight forwarders
// already work.  Air-gap policy doesn't apply because the sink
// only opens local file descriptors.
package cef

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func init() {
	output.DefaultSinkRegistry.Register("cef", NewFromSpec)
}

// Sink writes CEF lines to a single file.  Re-opens on Emit if the
// file has been rotated out from under us (logrotate copytruncate
// or move-and-create) — same posture as syslog/email sinks.
type Sink struct {
	name        string
	destination string // resolved file path
	vendor      string
	product     string
	version     string
	minSeverity output.Severity

	mu     sync.Mutex
	w      io.WriteCloser
	closed bool
}

// NewFromSpec builds a CEF sink from a SinkSpec.
func NewFromSpec(spec output.SinkSpec) (output.Sink, error) {
	dest, err := output.SinkConfigStringDefault(spec.Config, "destination", "")
	if err != nil {
		return nil, err
	}
	if dest == "" {
		return nil, errors.New("cef: config.destination is required (file:// URL)")
	}
	path, perr := resolveDestination(dest)
	if perr != nil {
		return nil, fmt.Errorf("cef: %w", perr)
	}

	vendor, _ := output.SinkConfigStringDefault(spec.Config, "vendor", "pg_hardstorage")
	product, _ := output.SinkConfigStringDefault(spec.Config, "product", "pg_hardstorage")
	version, _ := output.SinkConfigStringDefault(spec.Config, "version", "1")

	minSevStr, err := output.SinkConfigStringDefault(spec.Config, "min_severity", "notice")
	if err != nil {
		return nil, err
	}
	minSev, perr2 := output.ParseSeverity(minSevStr)
	if perr2 != nil {
		return nil, fmt.Errorf("cef: %w", perr2)
	}
	return &Sink{
		name:        spec.Name,
		destination: path,
		vendor:      vendor,
		product:     product,
		version:     version,
		minSeverity: minSev,
	}, nil
}

// resolveDestination accepts file:///... or a bare path.  The bare-
// path form is a convenience for operators who omit the scheme;
// any other scheme is refused (we don't quietly accept a
// network destination).
func resolveDestination(raw string) (string, error) {
	if strings.HasPrefix(raw, "/") {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse destination %q: %w", raw, err)
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("unsupported destination scheme %q (CEF sink only accepts file://)", u.Scheme)
	}
	if u.Path == "" {
		return "", fmt.Errorf("destination %q has empty path", raw)
	}
	return u.Path, nil
}

// Name implements output.Sink.
func (s *Sink) Name() string { return s.name }

// Open implements output.Sink.  Lazy: the file is opened on first
// Emit so a startup with no events doesn't touch the filesystem.
func (s *Sink) Open(_ context.Context, _ map[string]any) error { return nil }

// Emit implements output.Sink.
func (s *Sink) Emit(_ context.Context, ev *output.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("cef: sink closed")
	}
	if !ev.Severity.AtLeast(s.minSeverity) {
		return nil
	}
	if s.w == nil {
		f, err := os.OpenFile(s.destination, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("cef: open %s: %w", s.destination, err)
		}
		s.w = f
	}
	line := renderCEF(ev, s.vendor, s.product, s.version)
	if _, err := io.WriteString(s.w, line+"\n"); err != nil {
		// Drop the writer so the next Emit re-opens (handles
		// logrotate-style rotation).
		s.w.Close()
		s.w = nil
		return fmt.Errorf("cef: write: %w", err)
	}
	return nil
}

// Close implements output.Sink.
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.w != nil {
		err := s.w.Close()
		s.w = nil
		return err
	}
	return nil
}

// renderCEF formats one Event as a single-line CEF record.
//
// CEF:Version|Device Vendor|Device Product|Device Version|Device Event Class ID|Name|Severity|[Extension]
//
// The extension is "key=value" pairs separated by spaces, with
// special characters escaped per the CEF spec (\\, \=, \n, \r).
func renderCEF(ev *output.Event, vendor, product, version string) string {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "CEF:0|%s|%s|%s|%s|%s|%d|",
		cefHeaderEscape(vendor),
		cefHeaderEscape(product),
		cefHeaderEscape(version),
		cefHeaderEscape(eventClassID(ev)),
		cefHeaderEscape(eventName(ev)),
		mapSeverity(ev.Severity),
	)
	// CEF extension key=value pairs
	writeExt(bw, "rt", ev.GeneratedAt.UTC().Format(time.RFC3339))
	writeExt(bw, "cs1", ev.Schema)
	writeExt(bw, "cs1Label", "schema")
	writeExt(bw, "cs2", ev.Component)
	writeExt(bw, "cs2Label", "component")
	writeExt(bw, "cs3", ev.Op)
	writeExt(bw, "cs3Label", "op")
	if ev.Subject.Tenant != "" {
		writeExt(bw, "cs4", ev.Subject.Tenant)
		writeExt(bw, "cs4Label", "tenant")
	}
	if ev.Subject.Deployment != "" {
		writeExt(bw, "cs5", ev.Subject.Deployment)
		writeExt(bw, "cs5Label", "deployment")
	}
	if ev.Subject.BackupID != "" {
		writeExt(bw, "cs6", ev.Subject.BackupID)
		writeExt(bw, "cs6Label", "backup_id")
	}
	return strings.TrimRight(bw.String(), " ")
}

func eventClassID(ev *output.Event) string {
	if ev.Op != "" {
		return ev.Component + "." + ev.Op
	}
	return ev.Component
}

func eventName(ev *output.Event) string {
	if ev.Op != "" {
		return ev.Op
	}
	if ev.Component != "" {
		return ev.Component
	}
	return "event"
}

// mapSeverity maps the RFC 5424-aligned severity onto the
// 0..10 scale ArcSight expects.
func mapSeverity(s output.Severity) int {
	switch s {
	case output.SeverityEmergency, output.SeverityAlert, output.SeverityCritical:
		return 10
	case output.SeverityError:
		return 8
	case output.SeverityWarning:
		return 6
	case output.SeverityNotice:
		return 4
	case output.SeverityInfo:
		return 2
	case output.SeverityDebug:
		return 1
	}
	return 5
}

// cefHeaderEscape replaces the few characters CEF reserves in the
// pipe-delimited header: backslash → \\, pipe → \|.
func cefHeaderEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `|`, `\|`)
	return s
}

// cefExtEscape escapes the values of the extension's key=value
// pairs: backslash, equals, newline, carriage return.
func cefExtEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `=`, `\=`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	return s
}

func writeExt(bw *strings.Builder, key, value string) {
	if value == "" {
		return
	}
	fmt.Fprintf(bw, "%s=%s ", key, cefExtEscape(value))
}
