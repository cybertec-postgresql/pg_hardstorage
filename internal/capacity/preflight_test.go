package capacity_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/capacity"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// fakeFreeSpaceSP is a minimal StoragePlugin that satisfies the
// optional FreeSpaceAware interface. We only stub the shape
// Preflight uses; other methods are no-ops.
type fakeFreeSpaceSP struct {
	storage.StoragePlugin // embed for the no-op methods
	info                  storage.FreeSpaceInfo
	err                   error
}

func (f *fakeFreeSpaceSP) FreeSpace(ctx context.Context) (storage.FreeSpaceInfo, error) {
	return f.info, f.err
}

// fakeNoFreeSpaceSP doesn't implement FreeSpaceAware. The
// Preflight contract treats this as Unsupported.
type fakeNoFreeSpaceSP struct {
	storage.StoragePlugin
}

// TestPreflight_Pass: free ≥ projected × 1.10 → pass.
func TestPreflight_Pass(t *testing.T) {
	sp := &fakeFreeSpaceSP{info: storage.FreeSpaceInfo{TotalBytes: 1000, AvailableBytes: 500}}
	res, err := capacity.Preflight(context.Background(), sp, capacity.PreflightOptions{
		ProjectedBytes: 100,
	})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if res.Verdict != capacity.PreflightPass {
		t.Errorf("verdict = %s, want pass", res.Verdict)
	}
	if res.RequiredBytes != 110 {
		t.Errorf("RequiredBytes = %d, want 110 (100 × 1.10)", res.RequiredBytes)
	}
	if res.AvailableBytes != 500 {
		t.Errorf("AvailableBytes = %d, want 500", res.AvailableBytes)
	}
}

// TestPreflight_InsufficientSpace: free < projected × safety
// → insufficient_space verdict + descriptive Note.
func TestPreflight_InsufficientSpace(t *testing.T) {
	sp := &fakeFreeSpaceSP{info: storage.FreeSpaceInfo{TotalBytes: 1000, AvailableBytes: 50}}
	res, err := capacity.Preflight(context.Background(), sp, capacity.PreflightOptions{
		ProjectedBytes: 100,
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if res.Verdict != capacity.PreflightInsufficientSpace {
		t.Errorf("verdict = %s, want insufficient_space", res.Verdict)
	}
	if res.Note == "" {
		t.Errorf("expected populated Note for insufficient_space")
	}
}

// TestPreflight_BoundaryCondition: free == projected × safety
// is a pass (≥ comparison).
func TestPreflight_BoundaryCondition(t *testing.T) {
	sp := &fakeFreeSpaceSP{info: storage.FreeSpaceInfo{AvailableBytes: 110}}
	res, _ := capacity.Preflight(context.Background(), sp, capacity.PreflightOptions{
		ProjectedBytes: 100,
		SafetyFactor:   1.10,
	})
	if res.Verdict != capacity.PreflightPass {
		t.Errorf("free == required should pass; got %s", res.Verdict)
	}
}

// TestPreflight_Unsupported_BackendDoesntImplement: backend
// without FreeSpaceAware returns Unsupported.
func TestPreflight_Unsupported_BackendDoesntImplement(t *testing.T) {
	sp := &fakeNoFreeSpaceSP{}
	res, err := capacity.Preflight(context.Background(), sp, capacity.PreflightOptions{
		ProjectedBytes: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != capacity.PreflightUnsupported {
		t.Errorf("verdict = %s, want unsupported", res.Verdict)
	}
	if res.Note == "" {
		t.Error("Unsupported should populate Note")
	}
}

// TestPreflight_Unsupported_ExplicitFlag: a backend that
// implements the interface but reports Unsupported gets the
// same verdict.
func TestPreflight_Unsupported_ExplicitFlag(t *testing.T) {
	sp := &fakeFreeSpaceSP{info: storage.FreeSpaceInfo{Unsupported: true}}
	res, _ := capacity.Preflight(context.Background(), sp, capacity.PreflightOptions{
		ProjectedBytes: 100,
	})
	if res.Verdict != capacity.PreflightUnsupported {
		t.Errorf("verdict = %s, want unsupported", res.Verdict)
	}
}

// TestPreflight_ProbeError_Propagates: a FreeSpace probe error
// returns the error verbatim — caller decides fail-open vs
// fail-closed.
func TestPreflight_ProbeError_Propagates(t *testing.T) {
	sp := &fakeFreeSpaceSP{err: errors.New("statfs failed")}
	_, err := capacity.Preflight(context.Background(), sp, capacity.PreflightOptions{
		ProjectedBytes: 100,
	})
	if err == nil {
		t.Fatal("probe error should propagate")
	}
}

// TestPreflight_RejectsZeroProjection: ProjectedBytes ≤ 0 is
// a programmer error (we have nothing to compare against).
func TestPreflight_RejectsZeroProjection(t *testing.T) {
	sp := &fakeFreeSpaceSP{}
	for _, in := range []int64{0, -1, -100} {
		_, err := capacity.Preflight(context.Background(), sp, capacity.PreflightOptions{
			ProjectedBytes: in,
		})
		if err == nil {
			t.Errorf("ProjectedBytes=%d should error", in)
		}
	}
}

// TestPreflight_RejectsNilSP: nil StoragePlugin guard.
func TestPreflight_RejectsNilSP(t *testing.T) {
	_, err := capacity.Preflight(context.Background(), nil, capacity.PreflightOptions{
		ProjectedBytes: 100,
	})
	if err == nil {
		t.Error("nil sp should error")
	}
}

// TestPreflight_DefaultSafetyFactor: zero or negative
// SafetyFactor falls back to DefaultSafetyFactor (1.10).
func TestPreflight_DefaultSafetyFactor(t *testing.T) {
	sp := &fakeFreeSpaceSP{info: storage.FreeSpaceInfo{AvailableBytes: 105}}
	for _, in := range []float64{0, -1.0, -0.5} {
		res, _ := capacity.Preflight(context.Background(), sp, capacity.PreflightOptions{
			ProjectedBytes: 100,
			SafetyFactor:   in,
		})
		if res.SafetyFactor != capacity.DefaultSafetyFactor {
			t.Errorf("SafetyFactor=%v should default to %v; got %v", in, capacity.DefaultSafetyFactor, res.SafetyFactor)
		}
		// 105 < 110 (100 × 1.10) → insufficient.
		if res.Verdict != capacity.PreflightInsufficientSpace {
			t.Errorf("verdict = %s, want insufficient_space", res.Verdict)
		}
	}
}

// TestPreflight_CustomSafetyFactor: explicit SafetyFactor
// applied verbatim. 1.5 means "150% of projected required."
func TestPreflight_CustomSafetyFactor(t *testing.T) {
	sp := &fakeFreeSpaceSP{info: storage.FreeSpaceInfo{AvailableBytes: 140}}
	res, _ := capacity.Preflight(context.Background(), sp, capacity.PreflightOptions{
		ProjectedBytes: 100,
		SafetyFactor:   1.5,
	})
	if res.Verdict != capacity.PreflightInsufficientSpace {
		t.Errorf("verdict = %s, want insufficient (140 < 150)", res.Verdict)
	}
	if res.RequiredBytes != 150 {
		t.Errorf("RequiredBytes = %d, want 150", res.RequiredBytes)
	}
}

func TestPreflightRejectsNonFiniteSafetyFactor(t *testing.T) {
	sp := &fakeFreeSpaceSP{info: storage.FreeSpaceInfo{AvailableBytes: 1 << 20}}
	for _, factor := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := capacity.Preflight(context.Background(), sp, capacity.PreflightOptions{
			ProjectedBytes: 100,
			SafetyFactor:   factor,
		}); err == nil {
			t.Fatalf("non-finite SafetyFactor %v was accepted", factor)
		}
	}
}
