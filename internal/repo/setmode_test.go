package repo_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func TestSetMode_RoundTrip(t *testing.T) {
	ctx := context.Background()
	url := tempFileURL(t)
	if _, err := repo.Init(ctx, repo.InitOptions{URL: url}); err != nil {
		t.Fatalf("init: %v", err)
	}

	// A freshly-initialised repo has Mode unset (back-compat shape);
	// AssertWritable should treat it as writable.
	_, sp, err := repo.Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := repo.AssertWritable(ctx, sp); err != nil {
		t.Errorf("fresh repo should be writable; got %v", err)
	}
	sp.Close()

	// Flip to read-only.
	res, err := repo.SetMode(ctx, repo.SetModeOptions{URL: url, Mode: repo.ModeReadOnly})
	if err != nil {
		t.Fatalf("set-mode read-only: %v", err)
	}
	if res.Mode != repo.ModeReadOnly {
		t.Errorf("Mode = %q, want %q", res.Mode, repo.ModeReadOnly)
	}
	if res.PreviousMode != repo.ModeReadWrite {
		t.Errorf("PreviousMode = %q, want %q", res.PreviousMode, repo.ModeReadWrite)
	}

	// AssertWritable now returns ErrReadOnly.
	_, sp, err = repo.Open(ctx, url)
	if err != nil {
		t.Fatalf("open after set-mode: %v", err)
	}
	if err := repo.AssertWritable(ctx, sp); !errors.Is(err, repo.ErrReadOnly) {
		t.Errorf("expected ErrReadOnly; got %v", err)
	}
	sp.Close()

	// Flip back to read-write.
	if _, err := repo.SetMode(ctx, repo.SetModeOptions{URL: url, Mode: repo.ModeReadWrite}); err != nil {
		t.Fatalf("set-mode read-write: %v", err)
	}
	_, sp, err = repo.Open(ctx, url)
	if err != nil {
		t.Fatalf("open after flip back: %v", err)
	}
	if err := repo.AssertWritable(ctx, sp); err != nil {
		t.Errorf("re-opened read-write repo should be writable; got %v", err)
	}
	sp.Close()
}

func TestSetMode_RejectsBadMode(t *testing.T) {
	ctx := context.Background()
	url := tempFileURL(t)
	if _, err := repo.Init(ctx, repo.InitOptions{URL: url}); err != nil {
		t.Fatal(err)
	}
	_, err := repo.SetMode(ctx, repo.SetModeOptions{URL: url, Mode: repo.Mode("ZAlGo")})
	if err == nil {
		t.Fatal("set-mode with invalid mode should fail")
	}
}

func TestSetMode_NoRepo(t *testing.T) {
	ctx := context.Background()
	url := tempFileURL(t) // exists but has no HSREPO
	_, err := repo.SetMode(ctx, repo.SetModeOptions{URL: url, Mode: repo.ModeReadOnly})
	if !errors.Is(err, repo.ErrNotARepo) {
		t.Fatalf("expected ErrNotARepo; got %v", err)
	}
}

func TestMode_EffectiveAndIsValid(t *testing.T) {
	tests := []struct {
		m       repo.Mode
		wantEff repo.Mode
		valid   bool
	}{
		{"", repo.ModeReadWrite, true},
		{repo.ModeReadWrite, repo.ModeReadWrite, true},
		{repo.ModeReadOnly, repo.ModeReadOnly, true},
		{repo.Mode("nope"), repo.Mode("nope"), false},
	}
	for _, tc := range tests {
		if got := tc.m.Effective(); got != tc.wantEff {
			t.Errorf("Effective(%q) = %q; want %q", tc.m, got, tc.wantEff)
		}
		if got := tc.m.IsValid(); got != tc.valid {
			t.Errorf("IsValid(%q) = %v; want %v", tc.m, got, tc.valid)
		}
	}
}
