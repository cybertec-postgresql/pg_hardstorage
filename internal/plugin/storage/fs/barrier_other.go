//go:build !linux

package fs

import (
	"context"
	"errors"
	"fmt"
	stdfs "io/fs"
	"os"
	"path/filepath"
)

// Barrier is the portable (non-Linux) form of the deferred-chunk
// commit. Same three-step sequence as the Linux build — staging
// content durable, then publish to final keys, then directory
// entries durable (see barrier_linux.go for the rationale) — but it
// fsyncs each staging temp and each touched directory individually
// instead of issuing one syncfs.
func (p *Plugin) Barrier(ctx context.Context) error {
	p.mu.Lock()
	list := p.deferred
	p.deferred = nil
	p.mu.Unlock()
	if len(list) == 0 {
		return nil
	}
	// 1. Each staging temp's content durable.
	for i, dw := range list {
		if err := ctx.Err(); err != nil {
			p.requeue(list[i:])
			return err
		}
		if err := fsyncFile(dw.staging); err != nil {
			p.requeue(list[i:])
			return fmt.Errorf("fs: barrier fsync %q: %w", dw.staging, err)
		}
	}
	// 2. Publish each staged chunk to its final content key.
	rest, err := p.publishDeferred(ctx, list)
	if err != nil {
		p.requeue(rest)
		return err
	}
	// 3. The new directory entries (the links) durable.
	dirs := make(map[string]struct{})
	for _, dw := range list {
		dirs[filepath.Dir(dw.final)] = struct{}{}
	}
	for d := range dirs {
		if err := syncDir(d); err != nil {
			return fmt.Errorf("fs: barrier fsync dir %q: %w", d, err)
		}
	}
	return nil
}

// fsyncFile reopens path read-only and fsyncs its contents. A file
// deleted before the Barrier is a no-op success.
func fsyncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, stdfs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	return f.Sync()
}
