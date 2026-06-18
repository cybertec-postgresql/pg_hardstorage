package datadog

import "github.com/cybertec-postgresql/pg_hardstorage/internal/output"

// SetURLForTests overrides the apiURL on a Sink so unit tests can
// point at a httptest server rather than api.datadoghq.com.  The
// public Sink type is opaque outside the package; this hook is
// the seam tests use.
func SetURLForTests(s output.Sink, url string) {
	if ds, ok := s.(*Sink); ok {
		ds.apiURL = url
	}
}

// MapAlertType is the test-visible severity mapper.  Exposed so
// the sev → alert_type table is unit-testable without sending an
// HTTP request per case.
func MapAlertType(s output.Severity) string { return mapAlertType(s) }
