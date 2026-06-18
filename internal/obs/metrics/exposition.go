// exposition.go — render a Registry as Prometheus text exposition
// format (version 0.0.4) and serve it over HTTP.
package metrics

import (
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ContentType is the exposition format media type Prometheus scrapers
// and the pushgateway both accept.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// WriteExposition runs every scrape-time collector, then writes the whole
// registry in exposition format.  Families render in registration order;
// series within a family render in label-sorted order so the output is
// deterministic.
func (r *Registry) WriteExposition(w io.Writer) error {
	r.mu.RLock()
	collectors := append([]func(){}, r.collectors...)
	names := append([]string(nil), r.order...)
	fams := make([]*family, 0, len(names))
	for _, n := range names {
		fams = append(fams, r.families[n])
	}
	r.mu.RUnlock()

	// Collectors refresh scrape-time gauges (queue depths etc.) before we
	// snapshot.  They run outside the registry lock so a collector may
	// itself touch metrics without deadlocking.
	for _, fn := range collectors {
		fn()
	}

	bw := &strings.Builder{}
	for _, f := range fams {
		f.render(bw)
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

func (f *family) render(bw *strings.Builder) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if len(f.series) == 0 {
		// A family with no observed series still advertises its schema,
		// so a dashboard built against the name doesn't 404 on an idle
		// process.  HELP/TYPE with no samples is valid exposition.
		writeHelpType(bw, f.name, f.help, f.typ)
		return
	}
	writeHelpType(bw, f.name, f.help, f.typ)

	keys := append([]string(nil), f.keys...)
	sort.Strings(keys)

	for _, key := range keys {
		s := f.series[key]
		switch f.typ {
		case typeHistogram:
			f.renderHistogram(bw, s)
		default:
			bw.WriteString(f.name)
			writeLabels(bw, f.labels, s.labelValues, "", "")
			bw.WriteByte(' ')
			bw.WriteString(formatFloat(loadFloat(&s.bits)))
			bw.WriteByte('\n')
		}
	}
}

func (f *family) renderHistogram(bw *strings.Builder, s *series) {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	// Accumulate the non-cumulative per-bucket counts into the cumulative
	// `le` form Prometheus mandates.
	var cumulative uint64
	for i, ub := range f.buckets {
		cumulative += s.hbuckets[i]
		bw.WriteString(f.name)
		bw.WriteString("_bucket")
		writeLabels(bw, f.labels, s.labelValues, "le", formatFloat(ub))
		bw.WriteByte(' ')
		bw.WriteString(strconv.FormatUint(cumulative, 10))
		bw.WriteByte('\n')
	}
	// +Inf bucket == total count.
	bw.WriteString(f.name)
	bw.WriteString("_bucket")
	writeLabels(bw, f.labels, s.labelValues, "le", "+Inf")
	bw.WriteByte(' ')
	bw.WriteString(strconv.FormatUint(s.hcount, 10))
	bw.WriteByte('\n')

	bw.WriteString(f.name)
	bw.WriteString("_sum")
	writeLabels(bw, f.labels, s.labelValues, "", "")
	bw.WriteByte(' ')
	bw.WriteString(formatFloat(s.hsum))
	bw.WriteByte('\n')

	bw.WriteString(f.name)
	bw.WriteString("_count")
	writeLabels(bw, f.labels, s.labelValues, "", "")
	bw.WriteByte(' ')
	bw.WriteString(strconv.FormatUint(s.hcount, 10))
	bw.WriteByte('\n')
}

func writeHelpType(bw *strings.Builder, name, help string, typ metricType) {
	if help != "" {
		bw.WriteString("# HELP ")
		bw.WriteString(name)
		bw.WriteByte(' ')
		bw.WriteString(escapeHelp(help))
		bw.WriteByte('\n')
	}
	bw.WriteString("# TYPE ")
	bw.WriteString(name)
	bw.WriteByte(' ')
	bw.WriteString(string(typ))
	bw.WriteByte('\n')
}

// writeLabels emits `{a="1",b="2"}`.  An optional extra label (extraK/
// extraV) is appended — used for the histogram `le` label.  When there
// are no labels at all the braces are omitted.
func writeLabels(bw *strings.Builder, names, values []string, extraK, extraV string) {
	if len(names) == 0 && extraK == "" {
		return
	}
	bw.WriteByte('{')
	first := true
	for i, n := range names {
		if !first {
			bw.WriteByte(',')
		}
		first = false
		bw.WriteString(n)
		bw.WriteString(`="`)
		v := ""
		if i < len(values) {
			v = values[i]
		}
		bw.WriteString(escapeLabelValue(v))
		bw.WriteByte('"')
	}
	if extraK != "" {
		if !first {
			bw.WriteByte(',')
		}
		bw.WriteString(extraK)
		bw.WriteString(`="`)
		bw.WriteString(escapeLabelValue(extraV))
		bw.WriteByte('"')
	}
	bw.WriteByte('}')
}

// escapeLabelValue escapes the three characters the exposition format
// reserves inside a label value: backslash, double-quote, newline.
func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// escapeHelp escapes backslash and newline in HELP text (double-quotes
// are allowed unescaped in HELP).
func escapeHelp(v string) string {
	if !strings.ContainsAny(v, "\\\n") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	return r.Replace(v)
}

// formatFloat renders a float the way Prometheus expects: integers print
// without a trailing ".0", everything else uses the shortest round-trip
// form, and infinities map to the +Inf/-Inf tokens.
func formatFloat(v float64) string {
	switch {
	case v == float64(int64(v)) && v < 1e15 && v > -1e15:
		return strconv.FormatInt(int64(v), 10)
	default:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
}

// Handler returns an http.Handler that renders r on GET.  Non-GET
// requests get 405.  Render failures (a broken client connection) are
// swallowed — there's nothing useful to say to a scraper that hung up.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", ContentType)
		if req.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = r.WriteExposition(w)
	})
}
