// switchover.go — POST /switchover, the only mutating Patroni call
// pg_hardstorage makes.
//
// Everything else this client does is read-only: Cluster, Leader,
// History, IsLeaderCheck. Switchover moves a production primary, so it
// is deliberately in its own file with its own guard rails, and the
// only caller is the game-day scenario — an operator asking for a
// controlled failover drill.
//
// Patroni distinguishes switchover (planned, requires a healthy
// leader) from failover (unplanned, leader may be gone). We expose
// only switchover: a drill that needs the leader to be gone first is
// not a drill, it is an outage.

package patroni

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrSwitchoverRefused is returned when Patroni declines the
// switchover — most often HTTP 412, which it uses for "no candidate
// is healthy enough" and "the named leader is not the leader".
//
// This is separated from ErrUnexpected because refusal is an ANSWER,
// not a failure to communicate: the cluster is up, it evaluated the
// request, and it said no. A game-day drill that treats those the same
// reports an infrastructure problem when the truth is "your cluster
// has no healthy replica to promote", which is the finding.
var ErrSwitchoverRefused = errors.New("switchover refused by Patroni")

// SwitchoverRequest names the current leader and, optionally, the
// candidate to promote.
type SwitchoverRequest struct {
	// Leader is the CURRENT leader's member name. Patroni requires it
	// and rejects the call if it does not match — that is a guard
	// against racing a failover that already happened, so we pass it
	// through rather than letting it be omitted.
	Leader string `json:"leader"`

	// Candidate is the member to promote. Empty lets Patroni choose
	// the healthiest replica, which is what a drill usually wants.
	Candidate string `json:"candidate,omitempty"`
}

// Switchover asks Patroni to promote a replica and demote the current
// leader.
//
// Returns nil once Patroni has ACCEPTED the request. Acceptance is not
// completion: Patroni performs the promotion asynchronously, so a
// caller that needs the new leader must poll Cluster/Leader afterwards.
// The scenario in internal/gameday does exactly that, and treats a
// leader that never changes within its budget as a failure.
func (c *Client) Switchover(ctx context.Context, req SwitchoverRequest) error {
	if strings.TrimSpace(req.Leader) == "" {
		return fmt.Errorf("patroni: switchover requires the current leader's name")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("patroni: marshal switchover request: %w", err)
	}

	resp, err := c.doRaw(ctx, http.MethodPost, "/switchover", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	payload := strings.TrimSpace(string(buf[:n]))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("patroni: POST /switchover: %w (status %d)",
			ErrUnauthorized, resp.StatusCode)
	case resp.StatusCode == http.StatusPreconditionFailed:
		return fmt.Errorf("patroni: POST /switchover: %w (%.200q)",
			ErrSwitchoverRefused, payload)
	}
	return fmt.Errorf("patroni: POST /switchover: %w (status %d, body: %.200q)",
		ErrUnexpected, resp.StatusCode, payload)
}
