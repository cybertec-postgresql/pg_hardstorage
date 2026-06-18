package external_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/external"
)

// infiniteReader yields 'x' forever (no newline). Without a size cap,
// bufio.ReadString would buffer the entire stream into memory; the test
// would hang / OOM. With the cap it returns promptly with an error.
type infiniteReader struct{}

func (infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// TestServeRPC_BoundsRequestSize proves the untrusted-input read is
// bounded: a runaway plugin (or, here, the host side reading a runaway
// request) can't drive an unbounded allocation. The infinite reader
// would never terminate the old bufio.ReadString('\n'); the cap turns
// it into a prompt over-size error.
func TestServeRPC_BoundsRequestSize(t *testing.T) {
	var out bytes.Buffer
	err := external.ServeRPC(infiniteReader{}, &out, map[string]external.Handler{})
	if err == nil {
		t.Fatal("expected ServeRPC to reject an unbounded request")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("expected a size-cap error; got %v", err)
	}
}
