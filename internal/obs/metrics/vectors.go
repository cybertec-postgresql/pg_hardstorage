// vectors.go — the typed handles callers use: CounterVec / GaugeVec /
// HistogramVec and their single-series counterparts.  These are thin
// wrappers over *family; the registry owns all state.
package metrics

// CounterVec is a labelled monotonic counter.
type CounterVec struct{ f *family }

// Counter is one label-set's counter handle.
type Counter struct{ s *series }

// With returns the counter for the given label values (created on first
// use).  Arity must match the family's label schema.
func (c *CounterVec) With(values ...string) *Counter {
	return &Counter{c.f.seriesFor(values)}
}

// Inc adds 1.
func (c *Counter) Inc() { addFloat(&c.s.bits, 1) }

// Add adds delta (delta is expected to be non-negative for a counter; we
// don't enforce it — a negative is the caller's bug, and silently
// allowing it keeps the metrics path panic-free).
func (c *Counter) Add(delta float64) { addFloat(&c.s.bits, delta) }

// GaugeVec is a labelled value that can go up or down.
type GaugeVec struct{ f *family }

// Gauge is one label-set's gauge handle.
type Gauge struct{ s *series }

// With returns the gauge for the given label values.
func (g *GaugeVec) With(values ...string) *Gauge {
	return &Gauge{g.f.seriesFor(values)}
}

// Set replaces the gauge value.
func (g *Gauge) Set(v float64) { storeFloat(&g.s.bits, v) }

// Add adds delta (may be negative).
func (g *Gauge) Add(delta float64) { addFloat(&g.s.bits, delta) }

// Inc / Dec are the common ±1 shorthands.
func (g *Gauge) Inc() { addFloat(&g.s.bits, 1) }
func (g *Gauge) Dec() { addFloat(&g.s.bits, -1) }

// HistogramVec is a labelled histogram.
type HistogramVec struct{ f *family }

// Histogram is one label-set's histogram handle.
type Histogram struct {
	s *series
	f *family
}

// With returns the histogram for the given label values.
func (h *HistogramVec) With(values ...string) *Histogram {
	return &Histogram{s: h.f.seriesFor(values), f: h.f}
}

// Observe records one sample, incrementing the lowest bucket whose upper
// bound is ≥ v, plus the implicit +Inf bucket, and updating sum/count.
// Buckets here are stored non-cumulatively; WriteExposition accumulates
// them into the cumulative `le` form Prometheus expects.
func (h *Histogram) Observe(v float64) {
	h.s.hmu.Lock()
	defer h.s.hmu.Unlock()
	placed := false
	for i, ub := range h.f.buckets {
		if v <= ub {
			h.s.hbuckets[i]++
			placed = true
			break
		}
	}
	_ = placed // values above the last bound land only in +Inf (count)
	h.s.hsum += v
	h.s.hcount++
}
