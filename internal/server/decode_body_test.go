package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDecodeJSONBody_HappyPath: a normal-sized JSON body decodes
// into the destination struct without error.
func TestDecodeJSONBody_HappyPath(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"alice"}`))
	w := httptest.NewRecorder()
	var dst struct {
		Name string `json:"name"`
	}
	if err := decodeJSONBody(w, r, &dst); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dst.Name != "alice" {
		t.Errorf("name = %q", dst.Name)
	}
}

// TestDecodeJSONBody_EnforcesSizeCap: a body larger than
// MaxJSONRequestBytes is rejected by http.MaxBytesReader.  The
// audit fix: without the cap, a malicious client could
// post a multi-gigabyte body and the server would allocate
// while parsing.
//
// Test construction: a single huge JSON STRING value forces the
// decoder to read past the cap (a small JSON envelope with
// trailing whitespace doesn't, because Decode returns as soon
// as the value is closed).
func TestDecodeJSONBody_EnforcesSizeCap(t *testing.T) {
	// 2 MiB of 'x' inside a string — well past the 1 MiB cap.
	pad := bytes.Repeat([]byte("x"), MaxJSONRequestBytes+1024*1024)
	body := append(append([]byte(`{"name":"`), pad...), []byte(`"}`)...)
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	w := httptest.NewRecorder()
	var dst struct {
		Name string `json:"name"`
	}
	err := decodeJSONBody(w, r, &dst)
	if err == nil {
		t.Fatal("expected size-cap rejection")
	}
	// MaxBytesReader surfaces *http.MaxBytesError on overflow.
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) {
		t.Errorf("err = %v, want *http.MaxBytesError", err)
	}
}

// TestDecodeJSONBody_RejectsUnknownFields: DisallowUnknownFields
// is enabled so a client cannot stuff extra fields the server
// doesn't recognise — common signal of either a stale agent
// (older binary, has fields the server dropped) or a malicious
// client probing for type-confusion bugs.
func TestDecodeJSONBody_RejectsUnknownFields(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"alice","sneaky":"x"}`))
	w := httptest.NewRecorder()
	var dst struct {
		Name string `json:"name"`
	}
	if err := decodeJSONBody(w, r, &dst); err == nil {
		t.Fatal("expected unknown-field rejection")
	}
}

// TestDecodeJSONBody_EmptyBody returns io.EOF — callers that
// tolerate optional bodies (e.g. the cancel endpoint) check for
// it explicitly via errors.Is(err, io.EOF).
func TestDecodeJSONBody_EmptyBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	w := httptest.NewRecorder()
	var dst struct {
		Name string `json:"name"`
	}
	err := decodeJSONBody(w, r, &dst)
	if err == nil {
		t.Fatal("empty body should return io.EOF")
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF (so cancel-style optional-body callers can match it)", err)
	}
}
