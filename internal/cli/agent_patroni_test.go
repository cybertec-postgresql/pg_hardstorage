package cli

import (
	"strings"
	"testing"
	"time"
)

// TestSpliceDSNHostPort_URI: the canonical libpq URI form
// (postgres://user:pass@host:port/db?...) gets host:port
// replaced; everything else carries through.
func TestSpliceDSNHostPort_URI(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		host string
		port int
		want string
	}{
		{
			name: "URI-with-credentials",
			dsn:  "postgres://backup:secret@old-host:5432/postgres?sslmode=require",
			host: "new-host", port: 5432,
			want: "postgres://backup:secret@new-host:5432/postgres?sslmode=require",
		},
		{
			name: "URI-no-credentials",
			dsn:  "postgres://old-host:5432/postgres",
			host: "new-host", port: 5432,
			want: "postgres://new-host:5432/postgres",
		},
		{
			name: "URI-port-changes-too",
			dsn:  "postgres://user@old-host:5432/db",
			host: "new-host", port: 6432,
			want: "postgres://user@new-host:6432/db",
		},
		{
			name: "postgresql-scheme",
			dsn:  "postgresql://user@old-host:5432/db",
			host: "new-host", port: 5432,
			want: "postgresql://user@new-host:5432/db",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := spliceDSNHostPort(c.dsn, c.host, c.port)
			if got != c.want {
				t.Errorf("spliceDSNHostPort(%q, %q, %d) = %q, want %q",
					c.dsn, c.host, c.port, got, c.want)
			}
		})
	}
}

// TestSpliceDSNHostPort_KeyValue: the libpq key-value form
// gets host=/port= replaced, everything else preserved.
func TestSpliceDSNHostPort_KeyValue(t *testing.T) {
	got := spliceDSNHostPort(
		"host=old-host port=5432 user=backup dbname=postgres sslmode=require",
		"new-host", 6432)
	// Order isn't perfectly stable across the splice (we drop
	// the original host=/port= and append fresh ones at the end),
	// so check by fields.
	if !strings.Contains(got, "user=backup") {
		t.Errorf("user= should be preserved; got %q", got)
	}
	if !strings.Contains(got, "dbname=postgres") {
		t.Errorf("dbname= should be preserved; got %q", got)
	}
	if !strings.Contains(got, "sslmode=require") {
		t.Errorf("sslmode= should be preserved; got %q", got)
	}
	if !strings.Contains(got, "host=new-host") {
		t.Errorf("host= should be replaced; got %q", got)
	}
	if !strings.Contains(got, "port=6432") {
		t.Errorf("port= should be replaced; got %q", got)
	}
	// Old host/port must be gone.
	if strings.Contains(got, "host=old-host") {
		t.Errorf("old host= should be replaced, not appended: %q", got)
	}
	if strings.Contains(got, "port=5432") {
		t.Errorf("old port= should be replaced, not appended: %q", got)
	}
}

// TestSpliceDSNHostPort_KeyValueWithHostaddr: hostaddr= (used by
// libpq when an IP is supplied directly to skip DNS) is also
// replaced. We don't carry it through alongside a fresh host=
// because that would override the host we're trying to set.
func TestSpliceDSNHostPort_KeyValueWithHostaddr(t *testing.T) {
	got := spliceDSNHostPort(
		"hostaddr=10.0.0.1 port=5432 user=backup dbname=postgres",
		"new-host", 5432)
	if strings.Contains(got, "hostaddr=10.0.0.1") {
		t.Errorf("hostaddr= should be dropped on splice; got %q", got)
	}
	if !strings.Contains(got, "host=new-host") {
		t.Errorf("host= should be appended; got %q", got)
	}
}

// TestSpliceDSNHostPort_ReturnsEmptyOnUnparseable: a malformed
// URI returns "" so the Coordinator's dsn_build_failed event
// fires. We don't propagate the parse error — the Coordinator
// has its own error-event surface.
func TestSpliceDSNHostPort_ReturnsEmptyOnUnparseable(t *testing.T) {
	got := spliceDSNHostPort("postgres://[::malformed", "new-host", 5432)
	if got != "" {
		t.Errorf("expected empty for malformed URI; got %q", got)
	}
}

// TestSpliceDSNHostPort_RejectsEmpty: empty inputs short-circuit
// to "" so the Coordinator's dsn_build_failed event fires.
func TestSpliceDSNHostPort_RejectsEmpty(t *testing.T) {
	cases := []struct {
		dsn  string
		host string
		port int
	}{
		{"", "h", 5432},
		{"postgres://x@y/z", "", 5432},
		{"postgres://x@y/z", "h", 0},
	}
	for _, c := range cases {
		got := spliceDSNHostPort(c.dsn, c.host, c.port)
		if got != "" {
			t.Errorf("spliceDSNHostPort(%q, %q, %d) = %q, want empty",
				c.dsn, c.host, c.port, got)
		}
	}
}

// TestParsePatroniInterval: empty → zero (Coordinator default
// applies); valid → parsed; invalid → error.
func TestParsePatroniInterval(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, true},
		{"  ", 0, true},
		{"5s", 5 * time.Second, true},
		{"500ms", 500 * time.Millisecond, true},
		{"1m30s", 90 * time.Second, true},
		{"not-a-duration", 0, false},
		{"5", 0, false}, // no unit
	}
	for _, c := range cases {
		got, err := parsePatroniInterval(c.in)
		if c.ok && err != nil {
			t.Errorf("parsePatroniInterval(%q): unexpected err %v", c.in, err)
			continue
		}
		if !c.ok && err == nil {
			t.Errorf("parsePatroniInterval(%q): expected err, got %v", c.in, got)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("parsePatroniInterval(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
