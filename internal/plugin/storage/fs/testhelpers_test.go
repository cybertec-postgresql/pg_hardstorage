package fs

import (
	"bytes"
	"context"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// PutBytes is a tests-only ergonomic helper for callers that already
// have the full body in memory. It lives in *_test.go so it does not
// pollute the production API surface — the StoragePlugin contract is
// the only public way to put bytes.
//
// Defined in the (non-test) package so test files in the storage/fs
// package can call it via `p.PutBytes(...)` without having to import
// a sub-package; the file compiles only under `go test`.
func (p *Plugin) PutBytes(ctx context.Context, key string, body []byte, opts storage.PutOptions) (storage.PutResult, error) {
	if opts.ContentLength == 0 {
		opts.ContentLength = int64(len(body))
	}
	return p.Put(ctx, key, bytes.NewReader(body), opts)
}
