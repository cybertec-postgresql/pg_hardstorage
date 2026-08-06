package cli

// agent_patroni_events_test.go — the agent's Patroni startup events are
// the only signal an operator gets when a Patroni-enabled deployment
// fails to start following.
//
// Every one of these is emitted from startPatroniFollowers on a path
// that DOES NOT return an error: a deployment whose Patroni block is
// unusable is dropped and the rest of the fleet keeps running. That is
// the right posture — one bad deployment must not take down the agent —
// but it means the event is the entire failure report. If the event
// does not fire, or fires at the wrong severity, the deployment is
// silently not being followed and the WAL gap that eventually results
// has no antecedent in the logs.
//
// Before this file, 8 of these events were emitted by production code
// and named in no test anywhere in the tree. They are all `error`
// severity, which is what makes the omission matter: sinks filter on
// severity, and these are the ones meant to reach a human.
//
// The events assert component/op/severity, not message text, so
// rewording a message does not break the test while renaming the op —
// the string operators actually alert on — does.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	rendererjson "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/json"
)

// capturedEvent is the subset of the wire event this file asserts on.
//
// `severity` is a STRING on the wire, not the number the schema page
// used to claim. output.Severity carries UnmarshalText, so decoding
// into the typed field gives both the name and the ordering value. The
// first version of this file declared it `int`, which made every
// json.Decode fail and every case below report "no event emitted" —
// the assertions looked like product bugs when the fault was reading
// the documentation instead of the wire.
type capturedEvent struct {
	Severity     output.Severity `json:"severity"`
	SeverityName string          `json:"severity_name"`
	Component    string          `json:"component"`
	Op           string          `json:"op"`
}

// runPatroniStartup drives startPatroniFollowers over one deployment
// and returns every event it emitted.
func runPatroniStartup(t *testing.T, dep config.DeploymentConfig) []capturedEvent {
	t.Helper()
	var stdout, stderr bytes.Buffer
	d := output.NewDispatcher(rendererjson.New(), &stdout, &stderr)

	dones, _, err := startPatroniFollowers(context.Background(), d,
		map[string]config.DeploymentConfig{"db1": dep})
	// Every case here is a per-deployment refusal, which must NOT be
	// promoted to a fleet-wide failure.
	if err != nil {
		t.Fatalf("startPatroniFollowers returned a fatal error %v; a bad Patroni block on "+
			"one deployment must be reported as an event and skipped, not fail the agent", err)
	}
	for _, ch := range dones {
		close(ch)
	}
	_ = d.Close()

	// The JSON renderer pretty-prints, so each event spans many lines.
	// Decode the buffers as a STREAM of JSON values rather than one
	// value per line — a line-at-a-time parse silently yields nothing
	// and every assertion below would report "no event emitted" when
	// the event fired perfectly well.
	var out []capturedEvent
	for _, buf := range []*bytes.Buffer{&stdout, &stderr} {
		dec := json.NewDecoder(strings.NewReader(buf.String()))
		for {
			var ev capturedEvent
			if err := dec.Decode(&ev); err != nil {
				break
			}
			if ev.Op != "" {
				out = append(out, ev)
			}
		}
	}
	return out
}

func TestAgentPatroniStartupEvents(t *testing.T) {
	cases := []struct {
		name     string
		dep      config.DeploymentConfig
		wantOp   string
		wantSev  output.Severity
		wantName string
	}{
		{
			name: "no repo configured",
			dep: config.DeploymentConfig{
				Patroni: config.PatroniConfig{URL: "http://127.0.0.1:8008"},
			},
			wantOp: "patroni.skipped_no_repo", wantSev: output.SeverityWarning, wantName: "warning",
		},
		{
			name: "unparseable poll interval",
			dep: config.DeploymentConfig{
				Repo:    "file:///nonexistent-repo-for-this-test",
				Patroni: config.PatroniConfig{URL: "http://127.0.0.1:8008", Interval: "every-so-often"},
			},
			wantOp: "patroni.bad_interval", wantSev: output.SeverityError, wantName: "error",
		},
		{
			name: "slot and slots both set",
			dep: config.DeploymentConfig{
				Repo: "file:///nonexistent-repo-for-this-test",
				Patroni: config.PatroniConfig{
					URL:   "http://127.0.0.1:8008",
					Slot:  "one_slot",
					Slots: []config.PatroniSlot{{Name: "other", Role: "leader"}},
				},
			},
			wantOp: "patroni.slot_config_conflict", wantSev: output.SeverityError, wantName: "error",
		},
		{
			name: "slot role that is neither leader nor replica",
			dep: config.DeploymentConfig{
				Repo: "file:///nonexistent-repo-for-this-test",
				Patroni: config.PatroniConfig{
					URL:   "http://127.0.0.1:8008",
					Slots: []config.PatroniSlot{{Name: "s1", Role: "arbiter"}},
				},
			},
			wantOp: "patroni.bad_slot_role", wantSev: output.SeverityError, wantName: "error",
		},
		{
			name: "slot entry with no name",
			dep: config.DeploymentConfig{
				Repo: "file:///nonexistent-repo-for-this-test",
				Patroni: config.PatroniConfig{
					URL:   "http://127.0.0.1:8008",
					Slots: []config.PatroniSlot{{Name: "", Role: "leader"}},
				},
			},
			wantOp: "patroni.bad_slot_name", wantSev: output.SeverityError, wantName: "error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := runPatroniStartup(t, tc.dep)
			for _, ev := range events {
				if ev.Op != tc.wantOp {
					continue
				}
				if ev.Component != "agent" {
					t.Errorf("%s: component %q, want \"agent\"", tc.wantOp, ev.Component)
				}
				if ev.Severity != tc.wantSev || ev.SeverityName != tc.wantName {
					t.Errorf("%s: severity %d/%q, want %d/%q — sinks filter on this, so the "+
						"level decides whether an operator ever sees the deployment was dropped",
						tc.wantOp, int(ev.Severity), ev.SeverityName, int(tc.wantSev), tc.wantName)
				}
				return
			}
			var got []string
			for _, ev := range events {
				got = append(got, ev.Component+"."+ev.Op)
			}
			t.Errorf("no %q event emitted; got %v.\n\nThis deployment is dropped from the "+
				"fleet on a path that returns no error, so the event is the only report an "+
				"operator gets. Silence here means the deployment is not being followed and "+
				"nothing said so.", tc.wantOp, got)
		})
	}
}

// TestAgentPatroniStartupIsNonFatal pins the posture the events depend
// on: one unusable deployment must not stop the others from starting.
func TestAgentPatroniStartupIsNonFatal(t *testing.T) {
	var stdout, stderr bytes.Buffer
	d := output.NewDispatcher(rendererjson.New(), &stdout, &stderr)

	deps := map[string]config.DeploymentConfig{
		"broken": {
			Repo:    "file:///nonexistent-repo-for-this-test",
			Patroni: config.PatroniConfig{URL: "http://127.0.0.1:8008", Interval: "not-a-duration"},
		},
		"disabled": {Repo: "file:///nonexistent-repo-for-this-test"},
	}
	dones, count, err := startPatroniFollowers(context.Background(), d, deps)
	if err != nil {
		t.Fatalf("a single bad Patroni block failed the whole agent: %v", err)
	}
	for _, ch := range dones {
		close(ch)
	}
	_ = d.Close()
	if count != 0 {
		t.Errorf("started %d follower(s); the only Patroni-enabled deployment has an "+
			"unparseable interval and must be skipped", count)
	}
}
