package agent_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/agent"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/leakcheck"
)

type flakyExec struct{ n atomic.Int64 }

func (f *flakyExec) Execute(_ context.Context, j *agent.ControlPlaneJob, progress func(map[string]any)) (map[string]any, error) {
	progress(map[string]any{"job": j.ID})
	if f.n.Add(1)%3 == 0 {
		return nil, errors.New("executor failed")
	}
	return map[string]any{"ok": true}, nil
}

// The control-plane polling loop (heartbeat + claim + run + progress +
// complete + state watcher) run for thousands of jobs: goroutines, fds,
// connections and heap must be flat.
func TestLeak_ControlPlaneClientManyJobs(t *testing.T) {
	want := int64(600)
	if os.Getenv("PGHS_SCALE") == "1" {
		want = 20000
	}
	var claimSeq, completes atomic.Int64
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, body string) { w.WriteHeader(200); _, _ = w.Write([]byte(body)) }
	mux.HandleFunc("/v1/agents/heartbeat", func(w http.ResponseWriter, _ *http.Request) { ok(w, `{"result":{}}`) })
	mux.HandleFunc("/v1/jobs/claim", func(w http.ResponseWriter, _ *http.Request) {
		n := claimSeq.Add(1)
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, `{"result":{"id":"job-%d","kind":"backup","deployment":"db1"}}`, n)
	})
	mux.HandleFunc("/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/progress"):
			w.WriteHeader(http.StatusAccepted)
		case strings.HasSuffix(r.URL.Path, "/complete"):
			completes.Add(1)
			ok(w, `{"result":{}}`)
		default: // state GET
			ok(w, `{"result":{"state":"running","assigned_to":"leak-agent"}}`)
		}
	})
	hs := httptest.NewServer(mux)
	defer hs.Close()
	tr := &http.Transport{}
	hc := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	agent.SetStderrSink(devNull{})
	c := &agent.ControlPlaneClient{
		BaseURL: hs.URL, AgentID: "leak-agent", HTTPClient: hc,
		HeartbeatInterval: 3 * time.Millisecond, PollInterval: time.Millisecond, JitterFraction: -1,
		JobExecutor: &flakyExec{},
	}
	base := leakcheck.Take()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	var samples []uint64
	for completes.Load() < want {
		time.Sleep(100 * time.Millisecond)
		if int64(len(samples)+1)*want/8 <= completes.Load() {
			samples = append(samples, leakcheck.LiveHeap())
		}
	}
	cancel()
	<-done
	tr.CloseIdleConnections()
	hs.CloseClientConnections()
	t.Logf("%d jobs; heap KiB: %v", completes.Load(), func() (o []uint64) {
		for _, s := range samples {
			o = append(o, s>>10)
		}
		return
	}())
	if len(samples) >= 4 && samples[len(samples)-1] > samples[1]+8<<20 {
		t.Errorf("heap grows with job count: %v", samples)
	}
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{Goroutines: 2})
}

type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }
