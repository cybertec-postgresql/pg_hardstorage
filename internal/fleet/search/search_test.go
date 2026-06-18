package search_test

import (
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/fleet/search"
)

func TestParse_RejectsEmptyAndBadTokens(t *testing.T) {
	cases := map[string]string{
		"":                "empty",
		"   ":             "empty",
		"deployment":      "no colon",
		"deployment:":     "trailing colon",
		":db1":            "leading colon",
		"unknown:foo":     "unknown key",
		"pg_version:foo":  "non-int pg_version",
		"timeline:-1":     "negative timeline",
		"since:nonsense":  "bad duration",
		"before:also-bad": "bad before",
	}
	for q, why := range cases {
		if _, err := search.Parse(q); err == nil {
			t.Errorf("Parse(%q) — expected error (%s)", q, why)
		}
	}
}

func TestParse_AndCombination(t *testing.T) {
	q, err := search.Parse("deployment:db1 type:full pg_version:17")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	pass := &backup.Manifest{
		Deployment: "db1",
		Type:       "full",
		PGVersion:  17,
		StartedAt:  now,
	}
	if !q.Match(pass) {
		t.Error("matching manifest should match")
	}
	for _, m := range []*backup.Manifest{
		{Deployment: "db2", Type: "full", PGVersion: 17, StartedAt: now},
		{Deployment: "db1", Type: "incremental", PGVersion: 17, StartedAt: now},
		{Deployment: "db1", Type: "full", PGVersion: 16, StartedAt: now},
	} {
		if q.Match(m) {
			t.Errorf("non-matching manifest should not match: %+v", m)
		}
	}
}

func TestParse_DurationDays(t *testing.T) {
	q, err := search.Parse("since:7d")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if !q.Match(&backup.Manifest{StartedAt: now}) {
		t.Error("now should be within last 7d")
	}
	long := now.AddDate(0, 0, -10)
	if q.Match(&backup.Manifest{StartedAt: long}) {
		t.Error("10 days ago should not be within last 7d")
	}
}

func TestParse_RFC3339Absolute(t *testing.T) {
	q, err := search.Parse("since:2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if !q.Match(&backup.Manifest{StartedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}) {
		t.Error("2026-06-01 should be after 2026-01-01")
	}
	if q.Match(&backup.Manifest{StartedAt: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)}) {
		t.Error("2025-06-01 should not be after 2026-01-01")
	}
}

func TestAnd_String(t *testing.T) {
	q, err := search.Parse("deployment:db1 type:full")
	if err != nil {
		t.Fatal(err)
	}
	got := q.String()
	if !strings.Contains(got, `deployment="db1"`) || !strings.Contains(got, `type="full"`) {
		t.Errorf("String() = %q; missing predicates", got)
	}
}

func TestEmptyAnd_MatchesEverything(t *testing.T) {
	var q search.And
	if !q.Match(&backup.Manifest{Deployment: "anything"}) {
		t.Error("empty And should match everything")
	}
	if got := q.String(); got != "<all>" {
		t.Errorf("String() = %q; want <all>", got)
	}
}
