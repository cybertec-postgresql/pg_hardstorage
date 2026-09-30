package llmprovider_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/llmprovider"
)

// TestStallWatchdogUnblocksARealHTTPRead drives the watchdog through a
// real net/http response body.
//
// The stallReader unit tests used a fake body that ignores contexts,
// so they could only show the watchdog FIRED. That was not enough.
// The request was bound to the parent ctx while the watchdog cancelled
// a child context the read never observed: the timer fired and
// resp.Body.Read stayed blocked. Together with the removal of
// http.Client.Timeout, a connection that goes silent without a RST or
// FIN would hang the command forever. A server that sends one chunk
// and then says nothing reproduces that.
func TestStallWatchdogUnblocksARealHTTPRead(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		select { // then silence, like a black-holed connection
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	t.Setenv("PG_HARDSTORAGE_LLM_STALL_TIMEOUT", "300ms")
	t.Setenv("PG_HARDSTORAGE_LLM_FIRST_BYTE_TIMEOUT", "5s")

	p := newOpenAI()
	if err := p.Open(context.Background(), llmprovider.ProviderConfig{
		APIKey: "sk-test", Endpoint: srv.URL, Model: "m",
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		var last error
		for _, err := range p.Chat(context.Background(),
			[]llmprovider.Message{{Role: "user", Content: "hi"}}, nil) {
			if err != nil {
				last = err
			}
		}
		done <- last
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stream that went silent mid-answer ended without an error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the watchdog fired but the HTTP read never unblocked — Chat is hung on a silent connection")
	}
}
