package repo_test

import (
	"context"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestParseWORMRetention covers the supported unit suffixes + the
// validation paths.
func TestParseWORMRetention(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"7y", 7 * 365 * 24 * 60 * 60, false},
		{"30d", 30 * 24 * 60 * 60, false},
		{"24h", 24 * 60 * 60, false},
		{"60m", 60 * 60, false},
		{"1Y", 365 * 24 * 60 * 60, false}, // case-insensitive
		{"", 0, true},                     // empty
		{"7", 0, true},                    // no unit
		{"7w", 0, true},                   // unknown unit
		{"y", 0, true},                    // no number
		{"0d", 0, true},                   // must be > 0
		{"7y extra", 0, true},             // junk after digits
		{"a7d", 0, true},                  // alpha prefix
	}
	for _, c := range cases {
		got, err := repo.ParseWORMRetention(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseWORMRetention(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseWORMRetention(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseWORMRetention(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestMakeWORMPolicy validates the policy-construction wrapper.
func TestMakeWORMPolicy(t *testing.T) {
	p, err := repo.MakeWORMPolicy("compliance", "7y")
	if err != nil {
		t.Fatalf("MakeWORMPolicy: %v", err)
	}
	if p.Mode != "compliance" {
		t.Errorf("Mode=%q, want compliance", p.Mode)
	}
	if p.RetentionSeconds != 7*365*24*60*60 {
		t.Errorf("RetentionSeconds=%d, want %d", p.RetentionSeconds, 7*365*24*60*60)
	}
	if p.Retention != "7y" {
		t.Errorf("Retention=%q, want 7y (verbatim round-trip)", p.Retention)
	}

	// Empty (both flags omitted) → nil + nil.
	p, err = repo.MakeWORMPolicy("", "")
	if err != nil {
		t.Errorf("empty should not error: %v", err)
	}
	if p != nil {
		t.Errorf("empty should return nil; got %+v", p)
	}

	// Half-set → error.
	if _, err := repo.MakeWORMPolicy("compliance", ""); err == nil {
		t.Error("mode without retention should error")
	}
	if _, err := repo.MakeWORMPolicy("", "7y"); err == nil {
		t.Error("retention without mode should error")
	}

	// Bad mode.
	if _, err := repo.MakeWORMPolicy("loose", "7y"); err == nil {
		t.Error("invalid mode should error")
	}

	// Bad retention.
	if _, err := repo.MakeWORMPolicy("compliance", "tomorrow"); err == nil {
		t.Error("invalid retention should error")
	}
}

// TestWORMPolicy_RetainUntil computes the retention deadline.
func TestWORMPolicy_RetainUntil(t *testing.T) {
	p, _ := repo.MakeWORMPolicy("compliance", "30d")
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	until := p.RetainUntil(now)
	want := now.Add(30 * 24 * time.Hour)
	if !until.Equal(want) {
		t.Errorf("RetainUntil=%s, want %s", until, want)
	}

	// Nil policy returns zero.
	var zero *repo.WORMPolicy
	if z := zero.RetainUntil(now); !z.IsZero() {
		t.Errorf("nil policy RetainUntil should be zero; got %s", z)
	}

	// Empty mode policy returns zero.
	empty := &repo.WORMPolicy{}
	if z := empty.RetainUntil(now); !z.IsZero() {
		t.Errorf("empty policy RetainUntil should be zero; got %s", z)
	}
}

// TestWORMPolicy_Validate catches malformed combinations.
func TestWORMPolicy_Validate(t *testing.T) {
	cases := []struct {
		name    string
		p       *repo.WORMPolicy
		wantErr bool
	}{
		{"nil", nil, false},
		{"empty", &repo.WORMPolicy{}, false},
		{"valid compliance", &repo.WORMPolicy{Mode: "compliance", Retention: "7y", RetentionSeconds: 1000}, false},
		{"valid governance", &repo.WORMPolicy{Mode: "governance", Retention: "30d", RetentionSeconds: 1000}, false},
		{"bad mode", &repo.WORMPolicy{Mode: "loose", Retention: "7y", RetentionSeconds: 1000}, true},
		{"zero retention secs", &repo.WORMPolicy{Mode: "compliance", Retention: "7y", RetentionSeconds: 0}, true},
	}
	for _, c := range cases {
		err := c.p.Validate()
		if c.wantErr && err == nil {
			t.Errorf("%s: expected error", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// TestRepoInit_RecordsWORMInHSREPO: a repo init with a WORM policy
// stores it in HSREPO; a subsequent Open recovers it.
func TestRepoInit_RecordsWORMInHSREPO(t *testing.T) {
	root := t.TempDir()
	url := "file://" + root
	policy, _ := repo.MakeWORMPolicy("compliance", "7y")

	if _, err := repo.Init(context.Background(), repo.InitOptions{
		URL:  url,
		WORM: policy,
	}); err != nil {
		t.Fatal(err)
	}

	meta, sp, err := repo.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	if meta.WORM.IsZero() {
		t.Fatal("WORM should be recorded in HSREPO")
	}
	if meta.WORM.Mode != "compliance" {
		t.Errorf("Mode=%q, want compliance", meta.WORM.Mode)
	}
	if meta.WORM.Retention != "7y" {
		t.Errorf("Retention=%q, want 7y", meta.WORM.Retention)
	}
	if meta.WORM.RetentionSeconds != 7*365*24*60*60 {
		t.Errorf("RetentionSeconds=%d", meta.WORM.RetentionSeconds)
	}
}

// TestRepoInit_NoWORMByDefault: a repo init without WORM flags
// records nil/no policy.
func TestRepoInit_NoWORMByDefault(t *testing.T) {
	root := t.TempDir()
	url := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: url}); err != nil {
		t.Fatal(err)
	}
	meta, sp, err := repo.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	if !meta.WORM.IsZero() {
		t.Errorf("default repo should not have WORM; got %+v", meta.WORM)
	}
}

// TestRepoInit_RejectsInvalidWORM: a malformed WORMPolicy at init
// time fails fast (not silently written to HSREPO).
func TestRepoInit_RejectsInvalidWORM(t *testing.T) {
	root := t.TempDir()
	url := "file://" + root
	bad := &repo.WORMPolicy{Mode: "loose", RetentionSeconds: 1000}
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: url, WORM: bad}); err == nil {
		t.Error("invalid WORM mode should fail Init")
	}
}
