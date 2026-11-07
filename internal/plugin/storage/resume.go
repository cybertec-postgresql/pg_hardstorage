package storage

// resume.go — reading an object across a dropped connection.
//
// Get hands back one response body. The SDKs retry a REQUEST that fails,
// but once the body is flowing a reset is the caller's problem: Read
// returns the error and the object is half read. That is survivable for
// a chunk (fetch it again) and fatal for a manifest, which a restore
// streams for as long as the restore runs — hours, with the connection
// idle whenever the file writers are busy. One reset then ended a
// restore that was otherwise healthy (issue #97).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// OffsetReadAware is an OPTIONAL interface: GetFrom is Get starting at
// byte offset. Backends without it are read from the start and the
// prefix discarded, see GetFrom.
type OffsetReadAware interface {
	GetFrom(ctx context.Context, key string, offset int64) (io.ReadCloser, error)
}

// GetFrom returns key's bytes from offset on. With a backend that cannot
// seek it reads and discards the prefix, which costs the prefix's
// transfer but keeps the caller's stream going.
func GetFrom(ctx context.Context, sp StoragePlugin, key string, offset int64) (io.ReadCloser, error) {
	if offset <= 0 {
		return sp.Get(ctx, key)
	}
	if o, ok := sp.(OffsetReadAware); ok {
		return o.GetFrom(ctx, key, offset)
	}
	rc, err := sp.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if n, err := io.CopyN(io.Discard, rc, offset); err != nil {
		_ = rc.Close()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("storage: resume %s at byte %d: the object now ends at byte %d", key, offset, n)
		}
		return nil, fmt.Errorf("storage: resume %s at byte %d: %w", key, offset, err)
	}
	return rc, nil
}

// ResumeAttempts is how many times in a row GetResuming re-opens an
// object without reading a single new byte before it gives up. Progress
// resets the count, so a long stream survives any number of resets as
// long as each attempt gets somewhere.
const ResumeAttempts = 5

// resumeBackoff is the pause before re-opening after the nth failure in
// a row (n from 1). A var so tests do not sleep.
var resumeBackoff = func(n int) time.Duration {
	d := 250 * time.Millisecond << (n - 1)
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

// GetResuming is Get whose reader survives a failed Read: it re-opens
// the object at the byte it had reached and carries on. A missing
// object, a cancelled context and ResumeAttempts fruitless attempts in
// a row still end the stream with an error.
//
// The reader cannot tell that the object was REPLACED between two
// attempts. Use it where the content is authenticated afterwards (a
// manifest's signature and digest), not where the bytes are trusted as
// read.
func GetResuming(ctx context.Context, sp StoragePlugin, key string) (io.ReadCloser, error) {
	rc, err := sp.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return &resumingReader{ctx: ctx, sp: sp, key: key, rc: rc}, nil
}

type resumingReader struct {
	ctx   context.Context
	sp    StoragePlugin
	key   string
	rc    io.ReadCloser // nil between a failure and the re-open
	off   int64
	fails int   // failures since the last byte of progress
	cause error // the read error being recovered from
}

func (r *resumingReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if r.rc == nil {
			if err := r.reopen(); err != nil {
				return 0, err
			}
		}
		n, err := r.rc.Read(p)
		r.off += int64(n)
		if n > 0 {
			r.fails = 0
		}
		if err == nil || errors.Is(err, io.EOF) {
			return n, err
		}
		// ErrNotFound mid-stream is the object being deleted under the
		// reader; re-opening cannot bring it back.
		if r.ctx.Err() != nil || errors.Is(err, ErrNotFound) {
			return n, err
		}
		r.cause = err
		_ = r.rc.Close()
		r.rc = nil
		if n > 0 {
			// Hand over what arrived; the next Read re-opens.
			return n, nil
		}
	}
}

// reopen re-opens the object at r.off, pausing between attempts. It
// returns the error that ends the stream, wrapping the read error that
// started the recovery so the operator sees the cause, not only the
// last retry's symptom.
func (r *resumingReader) reopen() error {
	for {
		r.fails++
		if r.fails > ResumeAttempts {
			return fmt.Errorf("storage: read %s: gave up at byte %d after %d attempts without progress: %w",
				r.key, r.off, ResumeAttempts, r.cause)
		}
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-time.After(resumeBackoff(r.fails)):
		}
		rc, err := GetFrom(r.ctx, r.sp, r.key, r.off)
		if err == nil {
			r.rc = rc
			return nil
		}
		if r.ctx.Err() != nil || errors.Is(err, ErrNotFound) {
			return err
		}
		r.cause = err
	}
}

func (r *resumingReader) Close() error {
	if r.rc == nil {
		return nil
	}
	err := r.rc.Close()
	r.rc = nil
	return err
}
