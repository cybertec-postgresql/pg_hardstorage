package scp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// List is streamed from find's stdout: a consumer that stops early must not
// hang or poison the connection, and a full walk still returns everything.
func TestList_StreamsAndStopsEarly(t *testing.T) {
	srv := startExecServer(t)
	p := openExecPlugin(t, srv.addr, srv.hostPub)
	ctx := context.Background()
	const n = 40
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("chunks/k%03d", i)
		if _, err := p.Put(ctx, key, strings.NewReader("x"), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	seen := 0
	for _, err := range p.List(ctx, "chunks/") {
		if err != nil {
			t.Fatal(err)
		}
		seen++
		if seen == 3 {
			break
		}
	}
	if seen != 3 {
		t.Fatalf("early stop saw %d", seen)
	}
	total := 0
	for _, err := range p.List(ctx, "chunks/") {
		if err != nil {
			t.Fatal(err)
		}
		total++
	}
	if total != n {
		t.Fatalf("full list saw %d, want %d", total, n)
	}
	// A prefix that does not exist lists empty, not an error.
	for _, err := range p.List(ctx, "nope/") {
		t.Fatalf("unexpected entry or error: %v", err)
	}
}
