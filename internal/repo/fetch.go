package repo

// fetch.go — reading many chunks: several at once, delivered in order.
//
// Restore and verify read one chunk, then the next. Against an object
// store every chunk is a round trip of tens of milliseconds, so a
// restore ran at 25-36 chunks/s — 19-24 hours for 2.5 TiB — on a host
// whose CPU, disks and network were idle (issue #97). Restore writes 16
// files at once since issue #90, which helps exactly as far as there are
// 16 files left to write; verify had nothing.

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// DefaultFetchConcurrency is how many chunk reads a CAS keeps in flight
// for the bulk readers (restore, verify), and MaxFetchConcurrency the
// most an operator may ask for. Like DefaultUploadConcurrency it is not
// derived from GOMAXPROCS: a read waits on the storage round trip, and
// the scheduler bounds the decrypt/decompress/hash work to the cores
// there are. Memory is O(concurrency × chunk size).
const (
	DefaultFetchConcurrency = 16
	MaxFetchConcurrency     = 64
)

// ClampFetchConcurrency resolves a requested fetch concurrency: 0 (or
// negative) → DefaultFetchConcurrency, otherwise n capped at
// MaxFetchConcurrency. 1 reads one chunk at a time.
func ClampFetchConcurrency(n int) int {
	if n <= 0 {
		return DefaultFetchConcurrency
	}
	if n > MaxFetchConcurrency {
		return MaxFetchConcurrency
	}
	return n
}

// WithFetchConcurrency sets how many chunks FetchOrdered reads at once
// across all of its callers on this CAS (see ClampFetchConcurrency).
func WithFetchConcurrency(n int) CASOption {
	return func(c *CAS) { c.SetFetchConcurrency(n) }
}

// SetFetchConcurrency is WithFetchConcurrency for a CAS that is already
// built. Call it before the first FetchOrdered, not during one.
func (c *CAS) SetFetchConcurrency(n int) {
	c.fetchTokens = make(chan struct{}, ClampFetchConcurrency(n))
}

// FetchConcurrency reports the budget FetchOrdered works under.
func (c *CAS) FetchConcurrency() int { return cap(c.fetchTokens) }

// FetchOrdered reads the chunks hashes yields and calls fn once per
// chunk, in the order yielded, on the calling goroutine. Reads run
// ahead of fn, up to the CAS's fetch concurrency at once; that budget
// is shared by every FetchOrdered running on this CAS, so sixteen files
// restored side by side do not multiply it.
//
// fn receives the verified plaintext (GetChunkBytes) or the read's
// error, and decides: a non-nil return stops the walk and is returned.
// A chunk is held from the start of its read until its fn returns, so
// at most the fetch concurrency's worth of chunks is in memory.
//
// hashes is iterated on another goroutine.
func (c *CAS) FetchOrdered(ctx context.Context, hashes iter.Seq[Hash], fn func(h Hash, body []byte, err error) error) error {
	return EachOrdered(ctx, c.fetchTokens, hashes, c.GetChunkBytes, fn)
}

// NewFetchBudget returns a token pool for EachOrdered: n as resolved by
// ClampFetchConcurrency.
func NewFetchBudget(n int) chan struct{} {
	return make(chan struct{}, ClampFetchConcurrency(n))
}

// EachOrdered is the engine under FetchOrdered, generic in what is done
// per hash (verify's existence check Stats instead of reading). work
// runs for up to cap(tokens) hashes at once; fn sees the results in
// input order.
//
// A token is taken, in input order, before a hash's work starts and
// returned after its fn. Tokens held by one call therefore always cover
// a run of consecutive hashes starting at the one fn is waiting for, and
// that one is already running — so calls sharing a pool cannot starve
// each other into a standstill, whatever the pool's size.
func EachOrdered[T any](ctx context.Context, tokens chan struct{}, hashes iter.Seq[Hash],
	work func(context.Context, Hash) (T, error), fn func(Hash, T, error) error) error {
	type slot struct {
		h    Hash
		v    T
		err  error
		done chan struct{}
	}
	if cap(tokens) == 0 {
		// A nil or unbuffered pool would park the producer for good.
		tokens = make(chan struct{}, 1)
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// One queue entry per held token, so the producer's send never
	// blocks.
	queue := make(chan *slot, cap(tokens))
	var wg sync.WaitGroup
	complete := false // written before close(queue), read after it
	go func() {
		defer close(queue)
		for h := range hashes {
			if wctx.Err() != nil {
				return
			}
			select {
			case tokens <- struct{}{}:
			case <-wctx.Done():
				return
			}
			s := &slot{h: h, done: make(chan struct{})}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer close(s.done)
				s.v, s.err = work(wctx, s.h)
			}()
			queue <- s
		}
		complete = true
	}()

	var ferr error
	for s := range queue {
		<-s.done
		if ferr == nil {
			if err := fn(s.h, s.v, s.err); err != nil {
				ferr = err
				cancel() // stop the read-ahead; the rest is drained below
			}
		}
		var zero T
		s.v = zero
		<-tokens
	}
	wg.Wait()
	if ferr != nil {
		return ferr
	}
	if !complete {
		// The producer stopped early without fn failing: the caller's
		// context ended.
		return ctx.Err()
	}
	return nil
}

// chunkReadBackoff is the pause before the nth retry (n from 1) of a
// chunk whose body broke off mid-read: 200 ms, then 1 s, 2 s, 2 s. A
// var so tests do not sleep.
var chunkReadBackoff = func(n int) time.Duration {
	switch n {
	case 1:
		return 200 * time.Millisecond
	case 2:
		return time.Second
	default:
		return 2 * time.Second
	}
}

// chunkReadRetries is how often a chunk read that failed MID-BODY is
// repeated: five attempts in all, as storage.ResumeAttempts gives a
// manifest. The budget has to be judged per restore, not per chunk: a
// link that breaks one body in a hundred fails a given chunk three
// times in a row once in a million reads, which a restore of a million
// chunks (a quarter terabyte) would hit more often than not. Five
// attempts put that at one in ten thousand restores. Only a body that
// keeps breaking pays for the extra attempts; an open that fails is
// still not retried here.
const chunkReadRetries = 4

// errEnvelopeTooLarge marks a chunk object larger than any chunk can
// be: reading it again will not make it smaller.
var errEnvelopeTooLarge = errors.New("chunk envelope exceeds")

// readChunkEnvelope fetches a chunk's stored bytes, repeating a read
// whose body broke off.
//
// Only that is retried. A failed open is not: the SDKs already retry a
// request, and a missing or forbidden chunk must not cost every caller
// a second of backoff — verify walks a repository whose storage is gone
// one failing chunk after another. A body that stops halfway is the
// case nothing below us retries, and over the millions of reads of a
// large restore it does happen (issue #97).
func (c *CAS) readChunkEnvelope(ctx context.Context, hash Hash) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		rc, err := c.GetChunk(ctx, hash)
		if err != nil {
			return nil, err
		}
		envelope, rerr := readEnvelopeLimited(rc, MaxChunkEnvelopeBytes)
		_ = rc.Close()
		if rerr == nil {
			return envelope, nil
		}
		err = fmt.Errorf("cas: read chunk %s: %w", hash, rerr)
		if attempt >= chunkReadRetries || ctx.Err() != nil ||
			errors.Is(rerr, errEnvelopeTooLarge) || errors.Is(rerr, storage.ErrNotFound) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(chunkReadBackoff(attempt + 1)):
		}
	}
}
