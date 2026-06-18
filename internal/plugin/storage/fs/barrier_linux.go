//go:build linux

package fs

import (
	"context"
	"errors"
	"fmt"
	stdfs "io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// Barrier makes every preceding storage.DurabilityDeferred chunk
// crash-durable and publishes it to its final content key.
//
// The ordering is the correctness point:
//
//  1. syncfs(2) on the repo filesystem — every staging temp's
//     content is now on stable storage.
//  2. link each staging temp to its final content-addressed key
//     (publishDeferred). A chunk file appears at a real key ONLY
//     here — AFTER step 1 — so a crash can never leave a truncated
//     file at a key a later run's O_EXCL dedup would trust.
//  3. syncfs again — the new directory entries (the links) and the
//     staging-temp removals are durable.
//
// syncfs is one syscall for the whole filesystem no matter how many
// chunks were staged. On an early return the unpublished remainder
// is requeued so a retried Barrier completes the job.
func (p *Plugin) Barrier(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.root == "" {
		return errors.New("fs: Barrier called before Open")
	}
	p.mu.Lock()
	list := p.deferred
	p.deferred = nil
	p.mu.Unlock()
	if len(list) == 0 {
		return nil
	}
	if err := p.syncfsRoot(); err != nil {
		p.requeue(list)
		return err
	}
	rest, err := p.publishDeferred(ctx, list)
	if err != nil {
		p.requeue(rest)
		return err
	}
	if err := p.syncfsRoot(); err != nil {
		return err
	}
	return nil
}

// syncfsRoot flushes the entire filesystem the repository lives on.
func (p *Plugin) syncfsRoot() error {
	d, err := os.Open(p.root)
	if err != nil {
		// No repo directory yet means nothing was ever written.
		if errors.Is(err, stdfs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("fs: barrier open %q: %w", p.root, err)
	}
	defer d.Close()
	if err := unix.Syncfs(int(d.Fd())); err != nil {
		return fmt.Errorf("fs: barrier syncfs %q: %w", p.root, err)
	}
	return nil
}
