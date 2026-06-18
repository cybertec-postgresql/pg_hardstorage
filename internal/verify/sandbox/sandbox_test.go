package sandbox_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/verify/sandbox"
)

func TestVerify_RejectsEmptyDataDir(t *testing.T) {
	_, err := sandbox.Verify(context.Background(), sandbox.Options{})
	if err == nil {
		t.Fatal("Verify with empty DataDir should fail")
	}
}

func TestResultSchema(t *testing.T) {
	if sandbox.SchemaResult == "" {
		t.Error("SchemaResult should be a non-empty stable string")
	}
	// Pin the exact schema string — JSON consumers depend on
	// it round-tripping unchanged.
	want := "pg_hardstorage.verify.sandbox.v1"
	if sandbox.SchemaResult != want {
		t.Errorf("SchemaResult drift: got %q want %q", sandbox.SchemaResult, want)
	}
}

func TestBackendsRegistered(t *testing.T) {
	got := sandbox.Backends()
	sort.Strings(got)

	wantPresent := []string{"docker", "firecracker"}
	for _, w := range wantPresent {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("backend %q not registered; got %v", w, got)
		}
	}
}

func TestVerify_UnknownBackend(t *testing.T) {
	tmp := t.TempDir()
	_, err := sandbox.Verify(context.Background(), sandbox.Options{
		DataDir: tmp,
		Backend: "vmware-fusion-2007",
	})
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
	if !strings.Contains(err.Error(), "unknown backend") {
		t.Errorf("error should name the issue; got %v", err)
	}
	if !strings.Contains(err.Error(), "available:") {
		t.Errorf("error should list available backends; got %v", err)
	}
}

// TestVerify_FirecrackerStubRefuses asserts that when this
// binary is compiled WITHOUT -tags firecracker (the default),
// the Firecracker backend's Verify refuses with the
// rebuild-instructions error.  Skips itself when the cgo
// flavour is in play.
func TestVerify_FirecrackerStubRefuses(t *testing.T) {
	if sandbox.FirecrackerBuilt() {
		t.Skip("running under -tags firecracker; stub-refusal test does not apply")
	}
	tmp := t.TempDir()
	// Make a minimal data file so the validator passes its
	// existence check before reaching the stub refusal.
	imgPath := filepath.Join(tmp, "pgdata.img")
	if err := os.WriteFile(imgPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := sandbox.Verify(context.Background(), sandbox.Options{
		DataDir: imgPath,
		Backend: "firecracker",
	})
	if err == nil {
		t.Fatal("stub Firecracker backend should refuse")
	}
	if !errors.Is(err, sandbox.ErrFirecrackerNotBuilt()) {
		t.Errorf("expected ErrFirecrackerNotBuilt sentinel; got %v", err)
	}
	if !strings.Contains(err.Error(), "-tags firecracker") {
		t.Errorf("error should point at the rebuild step; got %v", err)
	}
	// Result is still emitted so the caller has a structured
	// record of "we tried, here's why we couldn't".
	if res == nil {
		t.Fatal("stub should still emit a Result")
	}
	if res.Backend != "firecracker" {
		t.Errorf("Result.Backend = %q, want firecracker", res.Backend)
	}
	if res.Schema != sandbox.SchemaResult {
		t.Errorf("Result.Schema drifted: %q", res.Schema)
	}
}

func TestParseMagic_AllVerdicts(t *testing.T) {
	cases := []struct {
		name        string
		console     string
		wantVerdict string
		wantDetail  string
		wantErr     bool
	}{
		{
			name: "pass via OK",
			console: "[    0.000000] Linux ...\n" +
				"running pg_verifybackup\n" +
				"__PG_HARDSTORAGE_VERIFY__:OK\n",
			wantVerdict: "PASS",
		},
		{
			name:        "pass via PASS",
			console:     "boot...\n__PG_HARDSTORAGE_VERIFY__:PASS\n",
			wantVerdict: "PASS",
		},
		{
			name:        "fail with detail",
			console:     "...\n__PG_HARDSTORAGE_VERIFY__:FAIL checksum mismatch on base/16384/2619\n",
			wantVerdict: "FAIL",
			wantDetail:  "checksum mismatch on base/16384/2619",
		},
		{
			name:        "skip with detail",
			console:     "...\n__PG_HARDSTORAGE_VERIFY__:SKIPPED no backup_manifest\n",
			wantVerdict: "SKIP",
			wantDetail:  "no backup_manifest",
		},
		{
			name:        "magic line embedded mid-stream",
			console:     "garbage [    1.234] more garbage __PG_HARDSTORAGE_VERIFY__:OK trailing\n",
			wantVerdict: "PASS",
		},
		{
			name:        "no magic line",
			console:     "[    0.000000] kernel ... but no magic line ever printed\n",
			wantVerdict: "UNKNOWN",
			wantErr:     true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sandbox.ParseMagicForTesting(c.console)
			if c.wantErr && got.Err == nil {
				t.Errorf("expected error; got verdict=%q detail=%q", got.Verdict, got.Detail)
			}
			if !c.wantErr && got.Err != nil {
				t.Errorf("unexpected error: %v", got.Err)
			}
			if got.Verdict != c.wantVerdict {
				t.Errorf("Verdict = %q want %q", got.Verdict, c.wantVerdict)
			}
			if got.Detail != c.wantDetail {
				t.Errorf("Detail = %q want %q", got.Detail, c.wantDetail)
			}
		})
	}
}

func TestStripControl(t *testing.T) {
	in := "hello\x00\x07world\nline2\n\x1b[31mred\x1b[0m"
	got := sandbox.StripControlForTesting(in)
	// All printable + \n preserved; control bytes (\x00,
	// \x07, \x1b) stripped.
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("printable text dropped: %q", got)
	}
	if strings.ContainsRune(got, '\x00') ||
		strings.ContainsRune(got, '\x07') ||
		strings.ContainsRune(got, '\x1b') {
		t.Errorf("control chars survived: %q", got)
	}
	if !strings.Contains(got, "\n") {
		t.Errorf("newlines should be preserved: %q", got)
	}
}

func TestValidateFirecrackerOpts(t *testing.T) {
	tmp := t.TempDir()
	good := func(name string) string {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	imgPath := good("pgdata.img")
	kernelPath := good("vmlinux")
	rootfsPath := good("rootfs.ext4")

	t.Run("happy path", func(t *testing.T) {
		err := sandbox.ValidateFirecrackerOptsForTesting(sandbox.Options{
			DataDir:           imgPath,
			FirecrackerKernel: kernelPath,
			FirecrackerRootfs: rootfsPath,
		})
		if err != nil {
			t.Errorf("happy path: %v", err)
		}
	})

	t.Run("rejects directory DataDir", func(t *testing.T) {
		err := sandbox.ValidateFirecrackerOptsForTesting(sandbox.Options{
			DataDir:           tmp,
			FirecrackerKernel: kernelPath,
			FirecrackerRootfs: rootfsPath,
		})
		if err == nil {
			t.Fatal("expected error for directory DataDir")
		}
		if !strings.Contains(err.Error(), "directory") {
			t.Errorf("error should mention directory; got %v", err)
		}
		if !strings.Contains(err.Error(), "block image") {
			t.Errorf("error should suggest block image; got %v", err)
		}
	})

	t.Run("missing kernel", func(t *testing.T) {
		err := sandbox.ValidateFirecrackerOptsForTesting(sandbox.Options{
			DataDir:           imgPath,
			FirecrackerKernel: "",
			FirecrackerRootfs: rootfsPath,
		})
		if err == nil {
			t.Fatal("expected error for empty kernel")
		}
	})

	t.Run("missing rootfs", func(t *testing.T) {
		err := sandbox.ValidateFirecrackerOptsForTesting(sandbox.Options{
			DataDir:           imgPath,
			FirecrackerKernel: kernelPath,
			FirecrackerRootfs: "",
		})
		if err == nil {
			t.Fatal("expected error for empty rootfs")
		}
	})

	t.Run("nonexistent kernel", func(t *testing.T) {
		err := sandbox.ValidateFirecrackerOptsForTesting(sandbox.Options{
			DataDir:           imgPath,
			FirecrackerKernel: "/nonexistent/kernel",
			FirecrackerRootfs: rootfsPath,
		})
		if err == nil {
			t.Fatal("expected error for missing kernel")
		}
	})
}
