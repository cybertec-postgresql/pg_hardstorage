// Package patroni implements a tiny client for the Patroni REST
// API. The plan calls Patroni's REST surface out as the
// operationally-correct path for leader-follow + slot-continuity
// in a Patroni-managed cluster:
//
//	"The agent does NOT connect to a hard-coded hostname. It polls
//	 Patroni's REST API (`GET /cluster`, `GET /leader`) and watches
//	 for leader changes via long-poll or DCS watch."
//
// What this package ships:
//
//   - Client.Cluster(ctx)      — full /cluster JSON, parsed
//   - Client.Leader(ctx)       — derived leader member from /cluster
//   - Client.IsLeaderCheck(ctx) — GET /leader (200 = current node is
//     the leader; 503 = not). Used by sidecar deployments that ARE
//     a Patroni node and want to know "is THIS node the primary?"
//   - Client.History(ctx)      — timeline-history events
//   - Client.Switchover(ctx)   — POST /switchover (deferred: the
//     gameday integration; v0.1 returns a structured "not yet
//     implemented" sentinel so callers detect cleanly)
//
// What this package does NOT ship:
//
//   - The agent-side leader-follow loop (long-poll on /cluster,
//     reconnect on leader change, re-issue START_REPLICATION).
//     That's the next piece on top of this client. Building the
//     client first lets the loop be tested in isolation.
//   - permanent_slots / synced-slots configuration helpers. Those
//     edit Patroni's DCS config and require an authenticated client
//     — separate scope.
//
// Authentication: Patroni's REST is HTTP basic auth or no auth.
// Cluster-level state endpoints (`/cluster`, `/leader`) are
// typically open; mutating endpoints (`/switchover`, `/restart`)
// require auth. We support optional basic-auth via WithAuth.
package patroni

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout is the per-request deadline. Operators can
// override via WithHTTPClient.
const DefaultTimeout = 5 * time.Second

// Client speaks Patroni REST against a single base URL.
type Client struct {
	baseURL  *url.URL
	http     *http.Client
	username string
	password string
}

// ClientOption tunes a Client at construction.
type ClientOption func(*Client)

// WithHTTPClient overrides the default http.Client. Useful for
// custom timeouts, proxy configuration, or instrumented round-trippers.
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *Client) { c.http = h }
}

// WithAuth sets HTTP basic-auth credentials. Required for mutating
// endpoints (switchover, restart) on Patroni's standard config.
// Read-only endpoints typically don't need auth but accept it.
func WithAuth(username, password string) ClientOption {
	return func(c *Client) {
		c.username = username
		c.password = password
	}
}

// NewClient constructs a Patroni REST client. baseURL must be an
// http://host:port form pointing at the Patroni REST endpoint of
// the cluster; the port is typically 8008.
func NewClient(baseURL string, opts ...ClientOption) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("patroni: baseURL is empty")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("patroni: parse baseURL %q: %w", baseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("patroni: baseURL scheme must be http or https; got %q", u.Scheme)
	}
	c := &Client{
		baseURL: u,
		http:    &http.Client{Timeout: DefaultTimeout},
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Cluster is the parsed /cluster response.
type Cluster struct {
	// Scope is the Patroni cluster name (the scope: in patroni.yml).
	Scope string `json:"scope,omitempty"`
	// Members lists every node Patroni knows about — primary +
	// replicas + sync-replicas, alive or dead.
	Members []Member `json:"members"`
}

// Member is one row in Cluster.Members.
type Member struct {
	Name     string `json:"name"`
	Role     string `json:"role"`     // leader | replica | sync_standby
	State    string `json:"state"`    // running | stopped | starting | stopping | crashed
	APIURL   string `json:"api_url"`  // http://host:port/patroni — not the libpq DSN
	Host     string `json:"host"`     // PG host
	Port     int    `json:"port"`     // PG port
	Timeline uint32 `json:"timeline"` // current TLI on this member
	// Lag in bytes from the leader. nil for the leader; 0 when a
	// replica has caught up. Patroni sometimes omits the field
	// entirely for stale members, and during a failover/transition
	// reports it as the string "unknown" instead of an integer —
	// see UnmarshalJSON, which folds both cases to nil.
	Lag *int64 `json:"lag,omitempty"`
}

// UnmarshalJSON tolerates Patroni's polymorphic `lag` field. The
// REST API normally reports lag as an integer (bytes behind the
// leader), but during a failover — exactly when a replica's
// upstream just changed — it emits the string "unknown" instead
// (issue #59: `json: cannot unmarshal string into ...
// Member.Lag of type int64`). A non-integer lag means "not known",
// the same signal as an omitted field, so we fold it to nil;
// pickReplica already treats nil lag as "no reported lag".
func (m *Member) UnmarshalJSON(data []byte) error {
	// Alias breaks the recursion (the alias has no UnmarshalJSON);
	// the outer Lag (json.RawMessage) shadows the embedded one for
	// the "lag" key, so we capture it raw and parse it ourselves.
	type alias Member
	aux := struct {
		Lag json.RawMessage `json:"lag"`
		*alias
	}{alias: (*alias)(m)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	m.Lag = nil
	if len(aux.Lag) > 0 && string(aux.Lag) != "null" {
		var n int64
		if err := json.Unmarshal(aux.Lag, &n); err == nil {
			m.Lag = &n
		}
		// Non-integer lag (e.g. "unknown") → leave nil = unknown.
	}
	return nil
}

// IsLeader reports whether this member is the current cluster
// leader. Patroni uses both "leader" and (rarely, in older versions)
// "master" — we accept either.
func (m *Member) IsLeader() bool {
	return m.Role == "leader" || m.Role == "master"
}

// Cluster fetches /cluster and parses it.
//
// Errors are wrapped with structured context so the caller can
// errors.Is them against the package sentinels:
//
//   - ErrUnreachable: connect / read failure
//   - ErrUnauthorized: HTTP 401 / 403
//   - ErrUnexpected: anything else non-2xx
func (c *Client) Cluster(ctx context.Context) (*Cluster, error) {
	body, err := c.do(ctx, "GET", "/cluster", nil)
	if err != nil {
		return nil, err
	}
	var cluster Cluster
	if err := json.Unmarshal(body, &cluster); err != nil {
		return nil, fmt.Errorf("patroni: decode /cluster: %w", err)
	}
	return &cluster, nil
}

// Leader fetches /cluster and returns the current leader member.
// Returns ErrNoLeader when no member has role=leader (the cluster
// has no current primary — DCS lock not held by anyone).
func (c *Client) Leader(ctx context.Context) (*Member, error) {
	cluster, err := c.Cluster(ctx)
	if err != nil {
		return nil, err
	}
	for i := range cluster.Members {
		if cluster.Members[i].IsLeader() {
			return &cluster.Members[i], nil
		}
	}
	return nil, ErrNoLeader
}

// IsLeaderCheck queries GET /leader on the configured base URL.
// Returns (true, nil) when the node Patroni is running on is the
// current leader (HTTP 200), (false, nil) when it isn't (HTTP 503),
// and a wrapped error for any other response. Used by sidecar
// deployments to know whether THIS node is the primary.
func (c *Client) IsLeaderCheck(ctx context.Context) (bool, error) {
	resp, err := c.doRaw(ctx, "GET", "/leader", nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusServiceUnavailable:
		return false, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return false, ErrUnauthorized
	}
	return false, fmt.Errorf("patroni: GET /leader: %w (status %d)",
		ErrUnexpected, resp.StatusCode)
}

// HistoryEvent is one entry in the /history response. Patroni
// returns these as a JSON array of arrays:
//
//	[[timeline, lsn, reason, timestamp, new_leader], ...]
//
// We unmarshal positionally and surface as named fields.
type HistoryEvent struct {
	Timeline  uint32    `json:"timeline"`
	SwitchLSN string    `json:"switch_lsn"`
	Reason    string    `json:"reason"`
	Timestamp time.Time `json:"timestamp"`
	NewLeader string    `json:"new_leader,omitempty"`
}

// History fetches /history and parses Patroni's positional-array
// shape into named-field structs.
func (c *Client) History(ctx context.Context) ([]HistoryEvent, error) {
	body, err := c.do(ctx, "GET", "/history", nil)
	if err != nil {
		return nil, err
	}
	// Patroni returns either:
	//   [[2, "0/15A2B388", "no recovery target specified", "...", "node-2"], ...]
	// or an empty array when there's no history.
	var raw [][]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("patroni: decode /history: %w", err)
	}
	out := make([]HistoryEvent, 0, len(raw))
	for _, row := range raw {
		ev, ok := parseHistoryRow(row)
		if !ok {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// parseHistoryRow extracts named fields from Patroni's positional
// [tli, lsn, reason, ts, new_leader] row. Tolerant: missing fields
// stay at zero-value rather than failing the whole parse.
func parseHistoryRow(row []any) (HistoryEvent, bool) {
	var ev HistoryEvent
	if len(row) < 1 {
		return ev, false
	}
	if v, ok := row[0].(float64); ok {
		ev.Timeline = uint32(v)
	}
	if len(row) > 1 {
		if s, ok := row[1].(string); ok {
			ev.SwitchLSN = s
		}
	}
	if len(row) > 2 {
		if s, ok := row[2].(string); ok {
			ev.Reason = s
		}
	}
	if len(row) > 3 {
		if s, ok := row[3].(string); ok {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				ev.Timestamp = t
			}
		}
	}
	if len(row) > 4 {
		if s, ok := row[4].(string); ok {
			ev.NewLeader = s
		}
	}
	return ev, true
}

// do issues a request and returns the body bytes on a 2xx response.
// Non-2xx maps to the package's error sentinels.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	resp, err := c.doRaw(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("patroni: read %s %s: %w", method, path, err)
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return bodyBytes, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("patroni: %s %s: %w (status %d)",
			method, path, ErrUnauthorized, resp.StatusCode)
	}
	return nil, fmt.Errorf("patroni: %s %s: %w (status %d, body: %.200q)",
		method, path, ErrUnexpected, resp.StatusCode, string(bodyBytes))
}

// BaseURL returns the configured base URL.  Exposed so callers
// emitting structured events (e.g. the wal-follower coordinator)
// can name the endpoint they tried in the event body — without it,
// a `patroni_poll_failed` event leaves the operator guessing which
// URL the agent was actually hitting.  Returns a copy; the caller
// cannot mutate Client's internal state.
func (c *Client) BaseURL() string {
	if c.baseURL == nil {
		return ""
	}
	u := *c.baseURL
	return u.String()
}

func (c *Client) doRaw(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	target := *c.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/" + strings.TrimLeft(path, "/")
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("patroni: build %s %s: %w", method, path, err)
	}
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// PRESERVE the underlying transport error.  Earlier
		// shape was `fmt.Errorf("patroni: %s %s: %w", method,
		// path, ErrUnreachable)` — which kept the sentinel for
		// errors.Is dispatch but THREW AWAY the real http.Client
		// error (DNS failure / connection refused / TLS error /
		// context deadline).  Operators got "REST endpoint
		// unreachable" with zero diagnostic info — see issue #74,
		// where a docker-compose haproxy front-end was probably
		// either misnamed in deployment config or genuinely
		// unreachable but the user couldn't tell which.
		//
		// New shape: %w wraps the sentinel (so errors.Is keeps
		// working) AND we append the real error.  Now the
		// operator sees, e.g.:
		//   patroni: GET /cluster: REST endpoint unreachable:
		//   Get "http://haproxy:8008/cluster": dial tcp: lookup
		//   haproxy on 127.0.0.11:53: no such host
		// — instantly actionable.
		return nil, fmt.Errorf("patroni: %s %s: %w: %v",
			method, path, ErrUnreachable, err)
	}
	return resp, nil
}

// Sentinels. errors.Is(err, ErrXxx) lets the caller dispatch on
// failure mode.  Sentinel strings deliberately omit the "patroni:"
// prefix because every wrap above already has it; including it
// here produced a doubled "patroni: ... : patroni: ..." in the
// rendered error (issue #74's cosmetic complaint, fixed alongside
// the diagnostic-info fix).
var (
	ErrUnreachable  = errors.New("REST endpoint unreachable")
	ErrUnauthorized = errors.New("unauthorized (HTTP 401/403)")
	ErrUnexpected   = errors.New("unexpected response")
	ErrNoLeader     = errors.New("no leader in cluster")
)
