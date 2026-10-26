package server

import (
	"context"
	"testing"
	"time"
)

// List must filter, order and truncate before cloning: a limit=2 query
// returns the two newest jobs, newest first.
func TestMemoryBackendList_LimitReturnsNewest(t *testing.T) {
	b := NewMemoryBackend()
	base := time.Now().UTC()
	for i, id := range []string{"a", "b", "c", "d"} {
		b.jobs[id] = &Job{ID: id, CreatedAt: base.Add(time.Duration(i) * time.Second)}
	}
	out, err := b.List(context.Background(), ListOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].ID != "d" || out[1].ID != "c" {
		t.Fatalf("got %+v", out)
	}
}
