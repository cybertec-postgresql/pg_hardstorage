package gcs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

func TestRetention_ReadsObjectRetention(t *testing.T) {
	until := time.Date(2026, 11, 9, 8, 0, 0, 52_000_000, time.UTC)
	cases := []struct {
		name string
		body string
		want storage.WORMMode
		zero bool
	}{
		{"locked", `{"bucket":"bkt","name":"k","retention":{"mode":"Locked","retainUntilTime":"2026-11-09T08:00:00.052Z"}}`, storage.WORMCompliance, false},
		{"unlocked", `{"bucket":"bkt","name":"k","retention":{"mode":"Unlocked","retainUntilTime":"2026-11-09T08:00:00.052Z"}}`, storage.WORMGovernance, false},
		{"no retention", `{"bucket":"bkt","name":"k"}`, storage.WORMNone, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					w.WriteHeader(http.StatusNotImplemented)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			got, mode, err := storage.RetentionOf(context.Background(), newFakeGCSPlugin(t, srv), "k")
			if err != nil {
				t.Fatal(err)
			}
			if mode != tc.want || got.IsZero() != tc.zero || (!tc.zero && !got.Equal(until)) {
				t.Errorf("Retention = %v %q, want %v %q", got, mode, until, tc.want)
			}
		})
	}
}

func TestRetention_MissingObjectIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":404,"message":"No such object"}}`)
	}))
	defer srv.Close()
	if _, _, err := storage.RetentionOf(context.Background(), newFakeGCSPlugin(t, srv), "k"); err == nil {
		t.Fatal("expected an error for a missing object")
	}
}
