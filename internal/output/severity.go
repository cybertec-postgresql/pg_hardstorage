// Package output is the structured-output spine of pg_hardstorage.
//
// Every user-visible piece of output — CLI command results, streaming
// progress, audit events, alerts, errors — is a strongly-typed value
// (Event or Result) flowing through one dispatcher with two plugin tiers:
//   - Renderer (synchronous, command-scoped, render to a Writer)
//   - Sink     (asynchronous, system-scoped, fan-out to external systems)
//
// This file defines Severity. We use the RFC 5424 model so syslog/CEF
// emission is direct and lossless. Emergency = 0 is most severe; Debug = 7
// is least. That ordering is convenient: a `min_severity` filter accepts
// events whose Severity <= the threshold.
package output

import (
	"fmt"
	"strings"
)

// Severity is an RFC 5424 syslog severity level.
type Severity int8

// RFC 5424 severity levels. Lower number = more severe.
const (
	SeverityEmergency Severity = iota // 0 - system unusable
	SeverityAlert                     // 1 - action must be taken immediately
	SeverityCritical                  // 2 - critical conditions
	SeverityError                     // 3 - error conditions
	SeverityWarning                   // 4 - warning conditions
	SeverityNotice                    // 5 - normal but significant
	SeverityInfo                      // 6 - informational
	SeverityDebug                     // 7 - debug-level
)

// severityNames is the canonical lowercase name for each level.
// We keep it as an array so lookup is O(1) and out-of-range is detectable.
var severityNames = [...]string{
	SeverityEmergency: "emergency",
	SeverityAlert:     "alert",
	SeverityCritical:  "critical",
	SeverityError:     "error",
	SeverityWarning:   "warning",
	SeverityNotice:    "notice",
	SeverityInfo:      "info",
	SeverityDebug:     "debug",
}

// String returns the canonical lowercase name (e.g. "warning").
// Unknown levels render as "severity(N)" so logs never lie about a value.
func (s Severity) String() string {
	if s < 0 || int(s) >= len(severityNames) {
		return fmt.Sprintf("severity(%d)", int8(s))
	}
	return severityNames[s]
}

// Valid reports whether s is one of the eight defined levels.
func (s Severity) Valid() bool {
	return s >= SeverityEmergency && s <= SeverityDebug
}

// AtLeast reports whether s is at least as severe as threshold.
// "More severe" means a lower numeric value, per RFC 5424.
//
//	SeverityError.AtLeast(SeverityWarning) == true   // error is more severe than warning
//	SeverityInfo.AtLeast(SeverityWarning)  == false  // info is less severe than warning
func (s Severity) AtLeast(threshold Severity) bool {
	return s <= threshold
}

// ParseSeverity is the inverse of String for the canonical names.
// It accepts case-insensitive input. Unknown names return an error so
// configuration loaders can surface typos rather than silently downgrade.
func ParseSeverity(name string) (Severity, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "emergency", "emerg":
		return SeverityEmergency, nil
	case "alert":
		return SeverityAlert, nil
	case "critical", "crit":
		return SeverityCritical, nil
	case "error", "err":
		return SeverityError, nil
	case "warning", "warn":
		return SeverityWarning, nil
	case "notice":
		return SeverityNotice, nil
	case "info", "informational":
		return SeverityInfo, nil
	case "debug":
		return SeverityDebug, nil
	}
	return SeverityInfo, fmt.Errorf("unknown severity %q", name)
}

// MarshalText implements encoding.TextMarshaler. We render severities as
// their string names in JSON / YAML / TOML so configs are human-friendly.
// The numeric value still ships in Event.Severity for machine consumers.
func (s Severity) MarshalText() ([]byte, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("invalid severity %d", int8(s))
	}
	return []byte(s.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Severity) UnmarshalText(b []byte) error {
	v, err := ParseSeverity(string(b))
	if err != nil {
		return err
	}
	*s = v
	return nil
}
