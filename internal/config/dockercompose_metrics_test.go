// dockercompose_metrics_test.go — pins that the eval stack actually
// EXPOSES the agent's Prometheus /metrics endpoint (issue #98).
//
// The metrics listener is opt-in: an agent started with an empty
// --metrics-listen serves no /metrics at all.  The eval stack
// originally ran `command: ["agent"]` with no flag and published no
// port, so `curl http://localhost:9187/metrics` failed with a
// connection error and Grafana had nothing to scrape — even though
// the listener code itself worked.
//
// This test reads the REAL docker-compose.yml and asserts the three
// things a reachable metrics surface needs:
//   1. the agent command passes a non-empty --metrics-listen bind,
//   2. that bind is on all interfaces (0.0.0.0 / :port), not the
//      default loopback that nothing outside the container can reach,
//   3. the chosen port is published to the host.
// A future edit that drops the flag or the port mapping fails here at
// PR time, not at `docker compose up` time.

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDockerComposeYAML_AgentExposesMetrics(t *testing.T) {
	repoRoot := repoRootFromTestDir(t)
	composePath := filepath.Join(repoRoot, "docker-compose.yml")
	body, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read %s: %v", composePath, err)
	}

	// command: is a string on some services (minio) and a list on
	// others (the agent), so decode it through a tolerant type.
	var doc struct {
		Services map[string]struct {
			Command stringOrSlice `yaml:"command"`
			Ports   []string      `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse docker-compose.yml as YAML: %v", err)
	}

	svc, ok := doc.Services["pg_hardstorage"]
	if !ok {
		t.Fatalf("docker-compose.yml has no pg_hardstorage service (have %v)", mapKeys(doc.Services))
	}

	// 1 + 2: the command must carry --metrics-listen with a non-empty,
	// all-interfaces bind. Accept both "--metrics-listen <addr>" and
	// "--metrics-listen=<addr>" forms.
	bind := metricsListenBind([]string(svc.Command))
	if bind == "" {
		t.Fatalf("pg_hardstorage agent command has no non-empty --metrics-listen (issue #98 regression); command=%v", svc.Command)
	}
	host, port, ok := strings.Cut(bind, ":")
	if !ok || port == "" {
		t.Fatalf("--metrics-listen %q is not a host:port bind", bind)
	}
	// An empty host (":9187") or 0.0.0.0 binds all interfaces; a
	// loopback bind (127.0.0.1/localhost) is unreachable from
	// Prometheus or the host, which is the whole bug.
	if host != "" && host != "0.0.0.0" {
		t.Fatalf("--metrics-listen %q binds %q, not all interfaces — Prometheus and the host cannot reach it (issue #98)", bind, host)
	}

	// 3: that port must be published to the host.
	if !portPublished(svc.Ports, port) {
		t.Fatalf("metrics port %q is not published in the pg_hardstorage `ports:` block %v — host `curl :%s/metrics` will fail (issue #98)", port, svc.Ports, port)
	}
}

// stringOrSlice decodes a YAML scalar OR sequence into a []string,
// matching how Docker Compose accepts `command:` in either form.
type stringOrSlice []string

func (s *stringOrSlice) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*s = stringOrSlice{n.Value}
		return nil
	}
	var xs []string
	if err := n.Decode(&xs); err != nil {
		return err
	}
	*s = xs
	return nil
}

// metricsListenBind returns the bind address passed via
// --metrics-listen in a compose command list, or "" if absent/empty.
func metricsListenBind(cmd []string) string {
	for i, tok := range cmd {
		if v, ok := strings.CutPrefix(tok, "--metrics-listen="); ok {
			return v
		}
		if tok == "--metrics-listen" && i+1 < len(cmd) {
			return cmd[i+1]
		}
	}
	return ""
}

// portPublished reports whether a "host:container" (or bare
// "container") ports entry exposes containerPort.
func portPublished(ports []string, containerPort string) bool {
	for _, p := range ports {
		// Strip an optional /proto suffix, then take the container side
		// (the last colon-separated field).
		spec := strings.SplitN(p, "/", 2)[0]
		fields := strings.Split(spec, ":")
		if fields[len(fields)-1] == containerPort {
			return true
		}
	}
	return false
}
