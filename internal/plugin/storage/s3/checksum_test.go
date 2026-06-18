// checksum_test.go — issue #86 regression coverage.
//
// aws-sdk-go-v2 v1.36+ started defaulting RequestChecksumCalculation
// to WhenSupported, which adds an
// `x-amz-sdk-checksum-algorithm: CRC32` header and a CRC32 body
// header to every PutObject.  Ceph-RGW-based S3-compatible
// services (Hetzner Object Storage, some MinIO configs, Backblaze
// B2 strict mode) reject those headers with `400 InvalidRequest`,
// which is the reporter's failure.
//
// The fix in s3.go sets the default to WhenRequired (only when an
// operation explicitly demands integrity).  This file pins the
// fix via two complementary tests:
//
//   1. headerCaptureServer drives a Put and asserts the outgoing
//      HTTP request does NOT carry the troublesome CRC32 header
//      under the default (WhenRequired) configuration.
//
//   2. hetznerSimServer simulates Hetzner's exact response:
//      400 InvalidRequest when the CRC32 header is present.  A
//      Put against it must succeed under the fix, and fail
//      explicitly when the operator opts back into
//      `?checksum=when_supported`.

package s3_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/s3"
)

// headerCaptureServer records every inbound request's headers so a
// test can inspect what the SDK actually sent on the wire.
type headerCaptureServer struct {
	mu       sync.Mutex
	requests []http.Header // one entry per PUT
}

func (h *headerCaptureServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Drain body so the SDK gets clean TCP semantics.
	_, _ = io.Copy(io.Discard, r.Body)
	defer r.Body.Close()

	if r.Method == http.MethodPut {
		h.mu.Lock()
		h.requests = append(h.requests, r.Header.Clone())
		h.mu.Unlock()
	}
	// Minimal success response.  No checksum headers in the
	// response so the SDK's response-validation path stays on
	// its happy path.
	w.WriteHeader(http.StatusOK)
}

// TestS3_PutObject_OmitsCRC32HeaderUnderDefaultConfig pins the issue
// #86 fix: under the default config (no ?checksum= param), the SDK
// must NOT add `x-amz-sdk-checksum-algorithm` or
// `x-amz-checksum-crc32` to PutObject requests.
func TestS3_PutObject_OmitsCRC32HeaderUnderDefaultConfig(t *testing.T) {
	cap := &headerCaptureServer{}
	srv := httptest.NewServer(cap)
	defer srv.Close()

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	p := &s3.Plugin{}
	urlStr := fmt.Sprintf("s3://test-bucket/test?endpoint=%s&region=us-east-1&path_style=true", srv.URL)
	u, err := url.Parse(urlStr)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if _, err := p.Put(context.Background(), "key", bytes.NewReader([]byte("hello")),
		storage.PutOptions{ContentLength: 5}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.requests) == 0 {
		t.Fatal("server saw no PUT — SDK never sent the request")
	}
	for i, h := range cap.requests {
		for _, banned := range []string{
			"X-Amz-Sdk-Checksum-Algorithm",
			"X-Amz-Checksum-Crc32",
			"X-Amz-Checksum-Crc32c",
			"X-Amz-Checksum-Sha1",
			"X-Amz-Checksum-Sha256",
			"X-Amz-Trailer", // SDK uses trailer for streaming checksums
		} {
			if got := h.Get(banned); got != "" {
				t.Errorf("request #%d carries %q=%q — issue #86 regression: SDK is sending a checksum header that Ceph-RGW services reject",
					i, banned, got)
			}
		}
	}
}

// hetznerSimServer simulates Hetzner Object Storage's rejection of
// SDK-default CRC32 headers.  Any PutObject with
// `x-amz-sdk-checksum-algorithm` OR `x-amz-trailer` set (the two
// shapes aws-sdk-go-v2 has used over the v1.36+ releases) returns
// 400 InvalidRequest; otherwise 200.
type hetznerSimServer struct {
	mu       sync.Mutex
	puts     int
	rejected int
}

func (h *hetznerSimServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	defer r.Body.Close()

	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusOK)
		return
	}

	h.mu.Lock()
	h.puts++
	if r.Header.Get("X-Amz-Sdk-Checksum-Algorithm") != "" ||
		r.Header.Get("X-Amz-Trailer") != "" {
		h.rejected++
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		_ = xml.NewEncoder(w).Encode(&errResp{
			Code:    "InvalidRequest",
			Message: "checksum algorithm CRC32 is not supported by this endpoint",
		})
		return
	}
	h.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

type errResp struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

// TestS3_PutObject_DefaultConfig_SucceedsAgainstHetznerSim is the
// reporter's exact scenario: a fresh Put against an endpoint that
// rejects SDK-default CRC32 must succeed under the fix.
func TestS3_PutObject_DefaultConfig_SucceedsAgainstHetznerSim(t *testing.T) {
	sim := &hetznerSimServer{}
	srv := httptest.NewServer(sim)
	defer srv.Close()

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	p := &s3.Plugin{}
	urlStr := fmt.Sprintf("s3://test-bucket/test?endpoint=%s&region=us-east-1&path_style=true", srv.URL)
	u, err := url.Parse(urlStr)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if _, err := p.Put(context.Background(), "key", bytes.NewReader([]byte("hello")),
		storage.PutOptions{ContentLength: 5}); err != nil {
		t.Fatalf("Put against Hetzner-sim: %v (issue #86 regression: SDK is still sending the CRC32 header)", err)
	}

	sim.mu.Lock()
	defer sim.mu.Unlock()
	if sim.puts == 0 {
		t.Error("Hetzner-sim saw no PUTs at all")
	}
	if sim.rejected != 0 {
		t.Errorf("Hetzner-sim rejected %d/%d PUTs for sending a CRC32 header — fix is not in effect", sim.rejected, sim.puts)
	}
}

// TestS3_PutObject_WhenSupportedOptIn_SendsCRC32Header covers the
// operator opt-back-in path: `?checksum=when_supported` restores the
// v1.36+ default-on behaviour, which adds a CRC32 hint to PutObject.
//
// We assert on the HEADER (the differentiable artefact at the wire)
// rather than the simulated rejection: aws-sdk-go-v2 1.41 puts the
// checksum into a trailer rather than a body header for PutObject,
// so a "trailer present" assertion is the right shape — Hetzner /
// Ceph reject on either.
func TestS3_PutObject_WhenSupportedOptIn_SendsCRC32Header(t *testing.T) {
	cap := &headerCaptureServer{}
	srv := httptest.NewServer(cap)
	defer srv.Close()

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	p := &s3.Plugin{}
	urlStr := fmt.Sprintf("s3://test-bucket/test?endpoint=%s&region=us-east-1&path_style=true&checksum=when_supported",
		srv.URL)
	u, err := url.Parse(urlStr)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if _, err := p.Put(context.Background(), "key", bytes.NewReader([]byte("hello")),
		storage.PutOptions{ContentLength: 5}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.requests) == 0 {
		t.Fatal("server saw no PUT")
	}
	// Under WhenSupported the SDK must signal a checksum
	// somewhere — header OR trailer.  The exact shape varies
	// between SDK versions; assert the union so the test stays
	// stable as the SDK evolves.
	found := false
	for _, h := range cap.requests {
		for _, key := range []string{
			"X-Amz-Sdk-Checksum-Algorithm",
			"X-Amz-Checksum-Crc32",
			"X-Amz-Trailer",
			"X-Amz-Decoded-Content-Length", // signals chunked-with-trailer
			"Content-Encoding",
		} {
			if v := h.Get(key); v != "" {
				if key == "Content-Encoding" && !strings.Contains(strings.ToLower(v), "aws-chunked") {
					continue
				}
				found = true
			}
		}
	}
	if !found {
		t.Errorf("under ?checksum=when_supported the SDK did NOT add any checksum-indicating header — the opt-in is a no-op (or the SDK changed shape).\nFull headers: %+v",
			cap.requests[0])
	}
}

// TestS3_Open_RejectsUnknownChecksumValue pins the input-validation
// shape — any ?checksum= value other than the documented two is a
// usage error, not a silent fallback.
func TestS3_Open_RejectsUnknownChecksumValue(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	p := &s3.Plugin{}
	// fake endpoint — Open shouldn't reach it because the
	// checksum validation fires first.
	u, _ := url.Parse("s3://b/p?endpoint=http://127.0.0.1:1&region=us-east-1&path_style=true&checksum=crc32")
	err := p.Open(context.Background(), storage.StorageConfig{URL: u})
	if err == nil {
		t.Fatal("expected an error for unknown checksum value")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("error %q does not mention the bad checksum value", err.Error())
	}
}
