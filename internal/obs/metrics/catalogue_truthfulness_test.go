// catalogue_truthfulness_test.go — meta-test pinning that the
// documented metric catalogue matches the runtime registry.
//
// Issue #98 was the eval stack documented `/metrics` but never
// enabled it.  The deeper class of bug is "the docs say one
// thing, the code does another" — and operators only find out
// when a Grafana panel goes blank or a Prometheus rule never
// fires.  This test makes the docs themselves the source of
// truth and refuses to compile when they drift.
//
// The contract:
//
//   - Every metric documented in metric-catalogue.md as
//     **Live** (or "Live (caveats)") MUST be registered in
//     this package's defaultReg.
//   - Every metric registered in defaultReg MUST appear in
//     metric-catalogue.md as Live.
//   - Metrics documented as **Reserved** are explicitly
//     allowed to NOT be registered yet (the SPEC-drift
//     contract — see SPEC_DRIFT.md #7/#8).
//
// Diff this test fails against → either the doc is lying or
// the code is lying.  Either way, the fix is one of:
//
//	(a) flip a Reserved tag to Live + wire a producer,
//	(b) keep the Reserved tag and don't claim it Live,
//	(c) rename the metric on both sides.
//
// The drift list lives in this test file (not in a YAML
// somewhere) because the test must compile against the SAME
// repo state that ships, so a doc-only change can't bypass
// the gate.
package metrics

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// metricsRefPath finds metric-catalogue.md relative to this
// test file so the test works from any cwd.
func metricsRefPath(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/obs/metrics → ../../../docs/reference/metric-catalogue.md
	return filepath.Clean(filepath.Join(filepath.Dir(here),
		"..", "..", "..", "docs", "reference", "metric-catalogue.md"))
}

// liveMetricsFromDoc parses metric-catalogue.md and returns the
// set of metric names appearing under a section header tagged
// "— **Live**".  Section headers may include a parenthetical
// caveat like "(lag gauges reserved)" — names inside those
// sections are returned as Live UNLESS the caveat narrows them.
// We handle the current caveat ("lag gauges reserved") by
// dropping any metric containing "_lag_" from the Live set
// when the section header carries that exact note.
func liveMetricsFromDoc(t *testing.T) (live map[string]bool, reserved map[string]bool) {
	t.Helper()
	body, err := os.ReadFile(metricsRefPath(t))
	if err != nil {
		t.Fatalf("read catalogue: %v", err)
	}
	live = map[string]bool{}
	reserved = map[string]bool{}

	sectionRe := regexp.MustCompile(`(?m)^## .*?— \*\*(Live|Reserved)\*\*(.*?)$`)
	nameRe := regexp.MustCompile(`pg_hardstorage_[a-z_]+`)

	matches := sectionRe.FindAllSubmatchIndex(body, -1)
	for i, m := range matches {
		state := string(body[m[2]:m[3]])
		caveat := string(body[m[4]:m[5]])
		start := m[1]
		end := len(body)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		section := body[start:end]
		for _, n := range nameRe.FindAll(section, -1) {
			name := string(n)
			if state == "Live" {
				// Caveat: "(lag gauges reserved)" tags `_lag_*`
				// names inside an otherwise-Live section as
				// Reserved.  If the catalogue grows additional
				// narrowing caveats they need to be parsed here
				// (failing them as Live would falsely accuse
				// the catalogue of lying).
				if strings.Contains(caveat, "lag gauges reserved") &&
					strings.Contains(name, "_lag_") {
					reserved[name] = true
					continue
				}
				live[name] = true
			} else {
				reserved[name] = true
			}
		}
	}
	return live, reserved
}

// registeredFamilies snapshots every metric name registered on
// the package-default registry.  Each catalogue.go init
// registers into Default(); we read the resulting registration
// order back through WriteExposition's deterministic output.
func registeredFamilies(t *testing.T) map[string]bool {
	t.Helper()
	var buf bytes.Buffer
	if err := Default().WriteExposition(&buf); err != nil {
		t.Fatalf("WriteExposition: %v", err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(buf.String(), "\n") {
		// Exposition emits "# HELP <name> ..." for every family,
		// even those with zero observed series — that's what we
		// match against here.  Parsing # HELP rather than the
		// sample lines means we pick up reserved-but-registered
		// families too (a registered family with no samples still
		// counts as "live in code").
		if !strings.HasPrefix(line, "# HELP ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		out[fields[2]] = true
	}
	return out
}

// TestMetricCatalogue_DocLiveIsRegistered: every metric the
// docs claim is Live must be in defaultReg.  A miss here means
// the docs are over-claiming — exactly the #98 failure mode.
func TestMetricCatalogue_DocLiveIsRegistered(t *testing.T) {
	live, _ := liveMetricsFromDoc(t)
	registered := registeredFamilies(t)
	var missing []string
	for name := range live {
		if !registered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("docs claim %d metric(s) Live that aren't registered (issue #98 class).\n"+
			"Either register the producer in internal/obs/metrics/catalogue.go OR re-tag\n"+
			"the metric as Reserved in docs/reference/metric-catalogue.md:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestMetricCatalogue_RegisteredIsDocumentedAsLive: every
// metric in defaultReg must appear in docs as Live.  A miss
// here means we ship a metric Prometheus consumers can scrape,
// but operators reading the docs won't know it exists — silent
// over-delivery, which becomes a backwards-compat trap when
// someone later renames the metric thinking nobody depended on
// it.
func TestMetricCatalogue_RegisteredIsDocumentedAsLive(t *testing.T) {
	live, reserved := liveMetricsFromDoc(t)
	registered := registeredFamilies(t)
	var undocumented, miscategorised []string
	for name := range registered {
		if live[name] {
			continue
		}
		if reserved[name] {
			miscategorised = append(miscategorised, name)
			continue
		}
		undocumented = append(undocumented, name)
	}
	sort.Strings(undocumented)
	sort.Strings(miscategorised)
	if len(undocumented) > 0 {
		t.Errorf("%d metric(s) registered but undocumented in docs/reference/metric-catalogue.md.\n"+
			"Operators scraping /metrics will see these but no doc explains them:\n  %s",
			len(undocumented), strings.Join(undocumented, "\n  "))
	}
	if len(miscategorised) > 0 {
		t.Errorf("%d metric(s) registered AND producing samples, but docs tag them Reserved.\n"+
			"Flip the doc tag to Live — they're not reserved if they emit:\n  %s",
			len(miscategorised), strings.Join(miscategorised, "\n  "))
	}
}

// TestMetricCatalogue_AllPrefixed: every registered metric
// must start with the documented `pg_hardstorage_` namespace.
// Prevents an accidental Default().Register() bypassing the
// `namespace+` convention catalogue.go uses.
func TestMetricCatalogue_AllPrefixed(t *testing.T) {
	registered := registeredFamilies(t)
	for name := range registered {
		if !strings.HasPrefix(name, "pg_hardstorage_") {
			t.Errorf("metric %q does not use the documented pg_hardstorage_ namespace", name)
		}
	}
}
