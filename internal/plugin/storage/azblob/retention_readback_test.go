package azblob

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// lockServer is a Blob service holding one blob under an immutability
// policy; it counts the policy writes it receives.
func lockServer(t *testing.T, until time.Time, mode string, sets *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch {
		case r.Method == http.MethodHead:
			if mode != "" {
				w.Header().Set("x-ms-immutability-policy-until-date", until.UTC().Format(http.TimeFormat))
				w.Header().Set("x-ms-immutability-policy-mode", mode)
			}
			w.Header().Set("Content-Length", "4")
			w.Header().Set("ETag", `"0x1"`)
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut && r.URL.Query().Get("comp") == "immutabilityPolicies":
			sets.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
}

func TestRetention_ReadsImmutabilityPolicy(t *testing.T) {
	until := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		azMode string
		want   storage.WORMMode
	}{
		{"locked", storage.WORMCompliance},
		{"unlocked", storage.WORMGovernance},
	} {
		var sets atomic.Int32
		srv := lockServer(t, until, tc.azMode, &sets)
		got, mode, err := storage.RetentionOf(context.Background(), newFakeAzPlugin(t, srv), "chunks/aa/aabb")
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v", tc.azMode, err)
		}
		if !got.Equal(until) || mode != tc.want {
			t.Errorf("%s: Retention = %v %q, want %v %q", tc.azMode, got, mode, until, tc.want)
		}
	}

	// No policy on the blob: a zero deadline, which covers nothing.
	var sets atomic.Int32
	srv := lockServer(t, time.Time{}, "", &sets)
	defer srv.Close()
	got, mode, err := storage.RetentionOf(context.Background(), newFakeAzPlugin(t, srv), "k")
	if err != nil || !got.IsZero() || mode != storage.WORMNone {
		t.Errorf("unlocked blob: Retention = %v %q %v, want zero", got, mode, err)
	}
}

// Azure replaces an Unlocked policy with whatever the next call asks
// for, earlier dates included. A base backup re-locking a chunk that
// `wal stream` holds longer would therefore SHORTEN the chunk's lock
// below what the WAL segment needs (S3 refuses the same request).
func TestSetRetention_UnlockedPolicyIsNeverShortened(t *testing.T) {
	held := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		azMode   string
		ask      time.Time
		mode     storage.WORMMode
		wantSets int32
	}{
		{"earlier deadline on an unlocked policy", "unlocked", held.Add(-6 * time.Hour), storage.WORMGovernance, 0},
		{"same deadline", "unlocked", held, storage.WORMGovernance, 0},
		{"governance asked, locked policy already longer", "locked", held.Add(-time.Hour), storage.WORMGovernance, 0},
		{"later deadline extends", "unlocked", held.Add(time.Hour), storage.WORMGovernance, 1},
		{"no policy yet", "", held, storage.WORMGovernance, 1},
		// Azure itself refuses to shorten a Locked policy; no read needed.
		{"compliance is passed through", "locked", held.Add(-time.Hour), storage.WORMCompliance, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sets atomic.Int32
			srv := lockServer(t, held, tc.azMode, &sets)
			defer srv.Close()
			if err := newFakeAzPlugin(t, srv).SetRetention(context.Background(), "chunks/aa/aabb", tc.ask, tc.mode); err != nil {
				t.Fatal(err)
			}
			if got := sets.Load(); got != tc.wantSets {
				t.Errorf("immutability policy written %d times, want %d", got, tc.wantSets)
			}
		})
	}
}
