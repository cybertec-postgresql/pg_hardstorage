// registry.go — a small, dependency-free Prometheus metric registry.
//
// We deliberately avoid github.com/prometheus/client_golang.  Like the
// soak-test pushgateway emitter (internal/testkit/validate/pushgateway.go),
// pg_hardstorage speaks the Prometheus text exposition format directly:
// it is a few hundred lines of stdlib, it adds zero dependencies to a
// binary that operators audit byte-for-byte, and the catalogue is small
// enough (a few dozen families) that a hand-rolled registry stays
// readable.  Operators wanting the full client_golang collector zoo wrap
// their own /metrics handler around the same process.
//
// The registry supports the three metric types the catalogue uses —
// counter, gauge, histogram — each as a labelled vector.  Updates are
// lock-free on the hot path (atomic add on a per-series value); only the
// first observation of a new label-set takes the registry's write lock to
// allocate the series.  Rendering walks every family in registration
// order so the exposition output is byte-stable across scrapes (handy for
// golden tests and for humans diffing two scrapes).
package metrics

import (
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// metricType enumerates the exposition `# TYPE` kinds we emit.
type metricType string

const (
	typeCounter   metricType = "counter"
	typeGauge     metricType = "gauge"
	typeHistogram metricType = "histogram"
)

// Registry holds a set of metric families plus any scrape-time
// collectors.  The zero value is not usable; construct with NewRegistry.
type Registry struct {
	mu         sync.RWMutex
	families   map[string]*family
	order      []string // family names in registration order
	collectors []func() // run at WriteExposition time, before render
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{families: map[string]*family{}}
}

// family is one metric name with a fixed type, help text, label schema,
// and (for histograms) bucket layout.  Its series map is keyed by the
// joined label values.
type family struct {
	name    string
	help    string
	typ     metricType
	labels  []string
	buckets []float64 // histogram only

	mu     sync.RWMutex
	series map[string]*series
	keys   []string // series keys in first-seen order, for stable render
}

// series is one label-set's value within a family.  Counters and gauges
// use bits (a float64 stored as its IEEE-754 bit pattern) so increments
// are a single atomic op.  Histograms additionally track per-bucket
// counts and the running sum; those are guarded by hmu because an
// observation touches several fields at once.
type series struct {
	labelValues []string

	bits uint64 // atomic float64 for counter/gauge

	hmu      sync.Mutex
	hbuckets []uint64 // count per bucket boundary (non-cumulative)
	hsum     float64
	hcount   uint64
}

// RegisterCounter declares a counter family.  Re-registering the same
// name returns the existing family (so independent call sites can each
// "declare" the metric they touch without coordinating).
func (r *Registry) RegisterCounter(name, help string, labels ...string) *CounterVec {
	return &CounterVec{r.register(name, help, typeCounter, labels, nil)}
}

// RegisterGauge declares a gauge family.
func (r *Registry) RegisterGauge(name, help string, labels ...string) *GaugeVec {
	return &GaugeVec{r.register(name, help, typeGauge, labels, nil)}
}

// RegisterHistogram declares a histogram family with the given upper
// bucket bounds (a +Inf bucket is always appended implicitly).  Bounds
// must be sorted ascending; they are sorted defensively here.
func (r *Registry) RegisterHistogram(name, help string, buckets []float64, labels ...string) *HistogramVec {
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	return &HistogramVec{r.register(name, help, typeHistogram, labels, b)}
}

func (r *Registry) register(name, help string, typ metricType, labels []string, buckets []float64) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.families[name]; ok {
		return f
	}
	f := &family{
		name:    name,
		help:    help,
		typ:     typ,
		labels:  append([]string(nil), labels...),
		buckets: buckets,
		series:  map[string]*series{},
	}
	r.families[name] = f
	r.order = append(r.order, name)
	return f
}

// AddCollector registers a callback invoked once per scrape, immediately
// before rendering.  Use it for gauges whose value is cheaper to read at
// scrape time than to track continuously (queue depths, registry sizes).
func (r *Registry) AddCollector(fn func()) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	r.collectors = append(r.collectors, fn)
	r.mu.Unlock()
}

// seriesFor finds or creates the series for the given label values.
func (f *family) seriesFor(values []string) *series {
	if len(values) != len(f.labels) {
		// Programmer error: the call site passed the wrong arity.  We
		// fold to a deterministic key rather than panic in a metrics
		// path — a mislabelled metric must never crash a backup.
		values = normalizeArity(values, len(f.labels))
	}
	key := encodeKey(values)

	f.mu.RLock()
	s := f.series[key]
	f.mu.RUnlock()
	if s != nil {
		return s
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if s = f.series[key]; s != nil {
		return s
	}
	s = &series{labelValues: append([]string(nil), values...)}
	if f.typ == typeHistogram {
		s.hbuckets = make([]uint64, len(f.buckets))
	}
	f.series[key] = s
	f.keys = append(f.keys, key)
	return s
}

func normalizeArity(values []string, n int) []string {
	out := make([]string, n)
	copy(out, values)
	return out
}

// encodeKey joins label values into a map key that can't collide across
// different label arrangements (the 0x1f unit separator never appears in
// a label value we generate).
func encodeKey(values []string) string {
	return strings.Join(values, "\x1f")
}

func addFloat(bits *uint64, delta float64) {
	for {
		old := atomic.LoadUint64(bits)
		nv := math.Float64frombits(old) + delta
		if atomic.CompareAndSwapUint64(bits, old, math.Float64bits(nv)) {
			return
		}
	}
}

func storeFloat(bits *uint64, v float64) {
	atomic.StoreUint64(bits, math.Float64bits(v))
}

func loadFloat(bits *uint64) float64 {
	return math.Float64frombits(atomic.LoadUint64(bits))
}
