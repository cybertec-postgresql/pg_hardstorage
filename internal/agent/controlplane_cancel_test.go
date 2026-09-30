package agent_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/agent"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// cancelObservingExecutor runs until its context is cancelled, emitting
// progress every tick if emit is set, and reports the cancellation.
type cancelObservingExecutor struct {
	started   chan struct{}
	cancelled chan struct{}
	emit      bool
}

func (b cancelObservingExecutor) Execute(ctx context.Context, _ *agent.ControlPlaneJob, progress func(map[string]any)) (map[string]any, error) {
	close(b.started)
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			close(b.cancelled)
			return nil, ctx.Err()
		case <-t.C:
			if b.emit {
				progress(map[string]any{"tick": true})
			}
		}
	}
}

// TestClient_CancelledJobStopsExecutor pins M39: cancelling a RUNNING
// job on the control plane must stop the agent's work. The agent
// ignored the 409 conflict.job_state its progress posts got back and
// nothing polled the job, so a cancelled restore kept writing.
func TestClient_CancelledJobStopsExecutor(t *testing.T) {
	for _, tc := range []struct {
		name string
		emit bool // true: observed via progress 409; false: via job-state poll
	}{{"silent executor", false}, {"progress-emitting executor", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
			if err != nil {
				t.Fatal(err)
			}
			hs := httptest.NewServer(s.Handler())
			defer hs.Close()
			job, err := s.Jobs().Enqueue(server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1"})
			if err != nil {
				t.Fatal(err)
			}
			ex := cancelObservingExecutor{started: make(chan struct{}), cancelled: make(chan struct{}), emit: tc.emit}
			c := &agent.ControlPlaneClient{
				BaseURL: hs.URL, AgentID: "a1", Deployments: []string{"db1"},
				HeartbeatInterval: time.Hour, PollInterval: 20 * time.Millisecond,
				JitterFraction: -1, JobExecutor: ex,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			go func() { _ = c.Run(ctx) }()

			select {
			case <-ex.started:
			case <-ctx.Done():
				t.Fatal("job never started")
			}
			if _, err := s.Jobs().Cancel(job.ID, "test"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ex.cancelled:
			case <-time.After(5 * time.Second):
				t.Fatal("executor kept running after the control plane cancelled the job")
			}
		})
	}
}
