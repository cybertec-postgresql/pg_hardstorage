// event.go — Schema constant + Subject struct: typed v1 event/result envelope shared across renderers.
package output

import (
	"errors"
	"fmt"
	"time"
)

// Schema is the wire-format identifier carried on every Event and Result.
//
// We commit to 24-month backward compatibility for documents bearing this
// schema. Breaking changes bump the major suffix (v2, v3, ...).
const Schema = "pg_hardstorage.v1"

// Subject is the "who/what does this event concern?" tuple. Every field is
// optional; render only what's set.
type Subject struct {
	Tenant     string `json:"tenant,omitempty"`
	Deployment string `json:"deployment,omitempty"`
	BackupID   string `json:"backup_id,omitempty"`
	Timeline   uint32 `json:"timeline,omitempty"`
	LSN        string `json:"lsn,omitempty"`
}

// IsZero is a small convenience used by the omitzero JSON tag and tests.
func (s Subject) IsZero() bool {
	return s == (Subject{})
}

// Suggestion carries the remediation an Event or Error proposes.
//
// Human is what we print to a TTY. Concept explains the underlying
// model (why this happened) so operators learn, not just fix.
// Command is a literal shell string the caller can copy or pipe.
// DocURL links to a runbook. All fields optional.
type Suggestion struct {
	Human   string `json:"human,omitempty"`
	Concept string `json:"concept,omitempty"`
	Command string `json:"command,omitempty"`
	DocURL  string `json:"doc_url,omitempty"`
}

// TraceContext is the W3C-style trace identifiers. Populated when the agent
// is part of a traced operation. Empty fields are omitted from output.
type TraceContext struct {
	TraceID string `json:"trace_id,omitempty"`
	SpanID  string `json:"span_id,omitempty"`
}

// Event is the streaming-output unit: one progress tick, one log line,
// one notification, one audit record. Renderers emit Events one-per-line
// (ndjson) or one-per-paragraph (text).
type Event struct {
	Schema       string       `json:"schema"`
	Severity     Severity     `json:"severity"`
	SeverityName string       `json:"severity_name"`
	Component    string       `json:"component,omitempty"`
	Op           string       `json:"op,omitempty"`
	Subject      Subject      `json:"subject,omitzero"`
	Body         any          `json:"body,omitempty"`
	Suggestion   *Suggestion  `json:"suggestion,omitempty"`
	Trace        TraceContext `json:"trace,omitzero"`
	GeneratedAt  time.Time    `json:"generated_at"`
}

// NewEvent returns an Event with Schema, severity name, and timestamp prefilled.
func NewEvent(severity Severity, component, op string) *Event {
	return &Event{
		Schema:       Schema,
		Severity:     severity,
		SeverityName: severity.String(),
		Component:    component,
		Op:           op,
		GeneratedAt:  time.Now().UTC(),
	}
}

// WithSubject is a small builder convenience.
func (e *Event) WithSubject(s Subject) *Event {
	e.Subject = s
	return e
}

// WithBody is a small builder convenience.
func (e *Event) WithBody(b any) *Event {
	e.Body = b
	return e
}

// snapshotForSinks returns a defensive copy safe to hand to the
// dispatcher's async sink goroutines. Event fan-out spawns one goroutine
// per sink while the foreground caller returns immediately and may mutate
// or reuse the original Event — its Body map in particular. The struct
// copy isolates the value fields (Subject / Trace / scalars); freezeValue
// deep-copies the Body's map/slice shapes so a caller mutating the
// original can't race an in-flight Emit reading it (data-race audit #1).
func (e *Event) snapshotForSinks() *Event {
	if e == nil {
		return nil
	}
	cp := *e
	cp.Body = freezeValue(e.Body)
	if e.Suggestion != nil {
		s := *e.Suggestion
		cp.Suggestion = &s
	}
	return &cp
}

// freezeValue deep-copies the map/slice shapes an Event Body is built
// from (map[string]any / []any, recursively). Other dynamic types —
// scalars, strings, opaque structs carried by value — are returned as-is;
// a caller must not mutate those in place after emitting.
func freezeValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = freezeValue(val)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, val := range t {
			s[i] = freezeValue(val)
		}
		return s
	default:
		return v
	}
}

// WithSuggestion is a small builder convenience.
func (e *Event) WithSuggestion(s *Suggestion) *Event {
	e.Suggestion = s
	return e
}

// Result is the one-shot command-output unit. A `pg_hardstorage status`
// invocation produces exactly one Result that wraps the actual payload.
//
// Either Result.Result or Result.Error is set, never both. The JSON
// renderer emits the entire Result object; the text renderer prints
// either the result body or the error message.
type Result struct {
	Schema      string    `json:"schema"`
	Command     string    `json:"command"`
	GeneratedAt time.Time `json:"generated_at"`
	Result      any       `json:"result,omitempty"`
	Error       *Error    `json:"error,omitempty"`
}

// NewResult returns a Result with Schema and timestamp prefilled.
func NewResult(command string) *Result {
	return &Result{
		Schema:      Schema,
		Command:     command,
		GeneratedAt: time.Now().UTC(),
	}
}

// WithBody sets the success body. Mutually exclusive with WithError.
func (r *Result) WithBody(body any) *Result {
	r.Result = body
	r.Error = nil
	return r
}

// WithError sets the failure body. Mutually exclusive with WithBody.
func (r *Result) WithError(err *Error) *Result {
	r.Error = err
	r.Result = nil
	return r
}

// IsError reports whether this Result carries an error payload.
func (r *Result) IsError() bool {
	return r != nil && r.Error != nil
}

// Error is a structured error that flows through the output system.
//
// It implements the standard `error` interface so commands can
// `return &output.Error{...}` from cobra RunE; the dispatcher walks the
// returned error chain (errors.As) to extract the structured form for
// JSON / NDJSON / sink emission and to derive the exit code.
type Error struct {
	Code       string      `json:"code"`
	Message    string      `json:"message"`
	Severity   Severity    `json:"severity,omitempty"`
	Subject    Subject     `json:"subject,omitzero"`
	Suggestion *Suggestion `json:"suggestion,omitempty"`
	// Cause is wrapped using fmt.Errorf("%w") semantics; not serialized
	// directly, but Unwrap returns it for errors.Is / errors.As chains.
	Cause error `json:"-"`
}

// NewError returns a structured Error with severity SeverityError. We
// don't allow Severity to default to its zero value (which is
// SeverityEmergency) — callers who need a more or less severe level
// must spell it out via WithSeverity.
func NewError(code, msg string) *Error {
	return &Error{
		Code:     code,
		Message:  msg,
		Severity: SeverityError,
	}
}

// WithSeverity overrides the default error-level severity. Useful for
// upgrading to critical/alert or downgrading to warning for soft errors.
func (e *Error) WithSeverity(s Severity) *Error {
	e.Severity = s
	return e
}

// WithSubject attaches the cluster/deployment/backup context.
func (e *Error) WithSubject(s Subject) *Error {
	e.Subject = s
	return e
}

// WithSuggestion attaches a remediation hint.
func (e *Error) WithSuggestion(s *Suggestion) *Error {
	e.Suggestion = s
	return e
}

// Wrap attaches a cause. Stored only in Cause (not serialized) so we can
// keep the public JSON clean while still supporting errors.Is/As chains.
func (e *Error) Wrap(cause error) *Error {
	e.Cause = cause
	return e
}

// Error implements the error interface. Format: "code: message".
// We deliberately don't include the cause in the string — JSON consumers
// see Code / Message as separate fields, and the cause chain is for
// errors.Is / errors.As, not for display.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Code == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the wrapped cause, if any, for errors.Is / errors.As.
func (e *Error) Unwrap() error {
	return e.Cause
}

// AsOutputError extracts a *Error from anywhere in an error chain.
// Returns (nil, false) if no structured error is present.
func AsOutputError(err error) (*Error, bool) {
	var oe *Error
	if errors.As(err, &oe) {
		return oe, true
	}
	return nil, false
}

// ToError converts any error to a *Error: structured errors pass through
// unchanged; others are wrapped with code "internal" at SeverityError.
// This is the single place where ad-hoc errors enter the structured world.
func ToError(err error) *Error {
	if err == nil {
		return nil
	}
	if oe, ok := AsOutputError(err); ok {
		return oe
	}
	return &Error{
		Code:     "internal",
		Message:  err.Error(),
		Severity: SeverityError,
		Cause:    err,
	}
}
