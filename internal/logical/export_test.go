package logical

import "context"

// SetRunOnce swaps the per-attempt pipeline for tests.
func SetRunOnce(r *Runner, f func(ctx context.Context, s *Stream, conn string) error) {
	r.runOnce = f
}
