package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/agent"
)

// TestClient_ClaimsAllExecutorKinds is the regression for bug #4: the
// claim body previously hardcoded kinds=["backup"], so restore/verify
// jobs sat queued forever even though the agent wired executors for
// them. The client must advertise exactly the kinds its executor can
// run — derived from the RouterExecutor's registered kinds.
func TestClient_ClaimsAllExecutorKinds(t *testing.T) {
	var gotKinds atomic.Value // []string

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agents/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	mux.HandleFunc("/v1/jobs/claim", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Kinds []string `json:"kinds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotKinds.Store(body.Kinds)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"notfound.no_jobs"}}`))
	})
	hs := httptest.NewServer(mux)
	defer hs.Close()

	// A RouterExecutor wired with backup+restore+verify — exactly the
	// production wiring in cli/agent.go.
	router := agent.NewRouterExecutor(map[string]agent.JobExecutor{
		"backup":  echoExecutor{},
		"restore": echoExecutor{},
		"verify":  echoExecutor{},
	})
	c := &agent.ControlPlaneClient{
		BaseURL:           hs.URL,
		AgentID:           "test-agent",
		HeartbeatInterval: 200 * time.Millisecond,
		PollInterval:      10 * time.Millisecond,
		JobExecutor:       router,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = c.Run(ctx)

	v := gotKinds.Load()
	if v == nil {
		t.Fatal("no claim observed")
	}
	got := append([]string(nil), v.([]string)...)
	sort.Strings(got)
	want := []string{"backup", "restore", "verify"}
	if len(got) != len(want) {
		t.Fatalf("claimed kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("claimed kinds = %v, want %v", got, want)
		}
	}
}

// TestClient_KindsFallBackToBackup: a non-kindLister executor keeps
// the historical {"backup"} advertisement so a bespoke single-purpose
// executor doesn't suddenly claim nothing.
func TestClient_KindsFallBackToBackup(t *testing.T) {
	var gotKinds atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agents/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	mux.HandleFunc("/v1/jobs/claim", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Kinds []string `json:"kinds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotKinds.Store(body.Kinds)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"notfound.no_jobs"}}`))
	})
	hs := httptest.NewServer(mux)
	defer hs.Close()

	c := &agent.ControlPlaneClient{
		BaseURL:           hs.URL,
		AgentID:           "test-agent",
		HeartbeatInterval: 200 * time.Millisecond,
		PollInterval:      10 * time.Millisecond,
		JobExecutor:       echoExecutor{}, // not a kindLister
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = c.Run(ctx)

	v := gotKinds.Load()
	if v == nil {
		t.Fatal("no claim observed")
	}
	got := v.([]string)
	if len(got) != 1 || got[0] != "backup" {
		t.Fatalf("fallback kinds = %v, want [backup]", got)
	}
}

// TestClient_HeartbeatsContinueDuringLongJob is the regression for
// bug #22: runOne used to execute synchronously inside the select
// loop, so heartbeats stopped for the job's whole duration and the
// agent dropped out of the fleet after HeartbeatTimeout. With the
// job running off the loop, heartbeats must keep firing while a long
// job is in flight.
func TestClient_HeartbeatsContinueDuringLongJob(t *testing.T) {
	var heartbeats atomic.Int32
	var jobIssued atomic.Bool
	var completes atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agents/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		heartbeats.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	mux.HandleFunc("/v1/jobs/claim", func(w http.ResponseWriter, _ *http.Request) {
		if jobIssued.Swap(true) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"notfound.no_jobs"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{"id":"job-slow","kind":"backup","deployment":"db1"}}`))
	})
	mux.HandleFunc("/v1/jobs/job-slow/complete", func(w http.ResponseWriter, _ *http.Request) {
		completes.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	hs := httptest.NewServer(mux)
	defer hs.Close()

	// The job blocks until its ctx is cancelled, simulating a long
	// backup. Heartbeats must continue while it runs.
	slow := &blockingExecutor{started: make(chan struct{})}

	c := &agent.ControlPlaneClient{
		BaseURL:           hs.URL,
		AgentID:           "test-agent",
		Deployments:       []string{"db1"},
		HeartbeatInterval: 20 * time.Millisecond,
		PollInterval:      10 * time.Millisecond,
		JobExecutor:       slow,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = c.Run(ctx); close(done) }()

	// Wait for the job to start executing.
	select {
	case <-slow.started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("job never started")
	}

	// While the job is blocked, heartbeats should keep firing. Give it
	// several heartbeat intervals.
	before := heartbeats.Load()
	time.Sleep(150 * time.Millisecond)
	after := heartbeats.Load()
	if after-before < 2 {
		cancel()
		<-done
		t.Fatalf("heartbeats stalled during job: before=%d after=%d (want steady ticking)", before, after)
	}

	cancel()
	<-done
}

// blockingExecutor blocks in Execute until ctx is cancelled, and
// signals when it has started. Used to simulate a long-running job.
type blockingExecutor struct {
	once    sync.Once
	started chan struct{}
}

func (b *blockingExecutor) Execute(ctx context.Context, _ *agent.ControlPlaneJob, _ func(map[string]any)) (map[string]any, error) {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return map[string]any{}, nil
}
