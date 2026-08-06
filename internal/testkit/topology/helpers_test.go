package topology

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// helpers_test.go — helpers shared across this package's harnesses.
//
// Deliberately UNTAGGED. chaos_soak_test.go and repro34_test.go each
// sit behind their own build tag now (they are Docker soaks and have no
// business in the default suite), and the moment they stopped sharing a
// tag they stopped being able to share a function: `tail` lived in
// repro34_test.go and `go vet -tags=chaos` broke with "undefined: tail".
//
// Anything used by more than one tagged harness belongs here.

// tail returns the last n bytes of s, elided at the front. Used to keep
// a failure message readable when a subprocess produced pages of output.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func (p *patroniLocalDocker) findLeaderName(ctx context.Context) string {
	for _, n := range p.nodes {
		body, err := httpGet(fmt.Sprintf("http://127.0.0.1:%d/cluster", n.patroniPort))
		if err != nil {
			continue
		}
		var cl struct {
			Members []struct {
				Name, Role, State string
			} `json:"members"`
		}
		if json.Unmarshal(body, &cl) != nil {
			continue
		}
		for _, m := range cl.Members {
			if m.Role == "leader" && m.State == "running" {
				return m.Name
			}
		}
	}
	return ""
}
func (p *patroniLocalDocker) switchover(ctx context.Context, leader string) error {
	// POST /switchover {leader} to any node's REST API; Patroni picks a
	// candidate. Body-less /failover is rejected, so name the leader.
	for _, n := range p.nodes {
		payload := fmt.Sprintf(`{"leader":%q}`, leader)
		req, _ := http.NewRequest("POST",
			fmt.Sprintf("http://127.0.0.1:%d/switchover", n.patroniPort),
			strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
		return fmt.Errorf("switchover HTTP %d: %s", resp.StatusCode, string(b))
	}
	return fmt.Errorf("no reachable patroni REST endpoint")
}

func httpGet(url string) ([]byte, error) {
	c := http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
