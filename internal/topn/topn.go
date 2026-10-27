// Package topn keeps the best N items of a stream in bounded memory. The
// per-kind record stores (integrity runs, approvals, JIT tokens, scans,
// reports, drill history) list objects that accumulate for the life of a
// repository; callers that only want the newest page must not pay heap for
// every record ever written.
package topn

import "sort"

// Collector accumulates items and, when a limit is set, periodically
// compacts to the best Limit items, so memory stays O(limit) however many
// items are offered.
type Collector[T any] struct {
	limit int
	less  func(a, b T) bool // a ranks strictly ahead of b
	items []T
}

// New returns a Collector. limit <= 0 keeps everything (unbounded, the
// legacy behaviour). less reports whether a ranks ahead of b.
func New[T any](limit int, less func(a, b T) bool) *Collector[T] {
	return &Collector[T]{limit: limit, less: less}
}

// Add offers one item.
func (c *Collector[T]) Add(v T) {
	c.items = append(c.items, v)
	if c.limit > 0 && len(c.items) >= 2*c.limit+256 {
		c.compact()
	}
}

func (c *Collector[T]) compact() {
	sort.SliceStable(c.items, func(i, j int) bool { return c.less(c.items[i], c.items[j]) })
	if c.limit > 0 && len(c.items) > c.limit {
		// Copy so the discarded tail can be collected.
		c.items = append([]T(nil), c.items[:c.limit]...)
	}
}

// Result returns the best items, best first, at most Limit of them.
func (c *Collector[T]) Result() []T {
	c.compact()
	return c.items
}
