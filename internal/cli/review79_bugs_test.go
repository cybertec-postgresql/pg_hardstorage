package cli

import (
	"context"
	"errors"
	"iter"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// mustURL parses raw or fails the test.
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// setTestKeyringHome points paths.Resolve at a fresh HOME and
// pre-generates the keyring so loadVerifier() succeeds inside a
// package-internal test.
func setTestKeyringHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := keystore.LoadOrGenerate(p.Keyring.Value); err != nil {
		t.Fatal(err)
	}
}

// --- Bug 70: spliceDSNHostPort must not corrupt quoted key-value DSNs ---

// TestSpliceDSNHostPort_QuotedValueWithSpaces is the #70 regression:
// a libpq key-value DSN whose password (or any value) is single-quoted
// and contains spaces must survive the host/port splice intact. The
// old strings.Fields tokeniser split "password='a b'" across two
// tokens and corrupted the DSN on a Patroni leader change.
func TestSpliceDSNHostPort_QuotedValueWithSpaces(t *testing.T) {
	got := spliceDSNHostPort(
		"host=old port=5432 user=backup password='a b c' dbname=postgres",
		"new-host", 6432)
	if !strings.Contains(got, "password='a b c'") {
		t.Errorf("quoted password with spaces must survive intact; got %q", got)
	}
	if !strings.Contains(got, "user=backup") || !strings.Contains(got, "dbname=postgres") {
		t.Errorf("other pairs must be preserved; got %q", got)
	}
	if !strings.Contains(got, "host=new-host") || !strings.Contains(got, "port=6432") {
		t.Errorf("host/port must be replaced; got %q", got)
	}
	if strings.Contains(got, "host=old") || strings.Contains(got, "port=5432") {
		t.Errorf("old host/port must be dropped; got %q", got)
	}
	// Re-parsing the spliced DSN must yield exactly one password pair
	// with the space-containing value — i.e. it wasn't split.
	pairs, ok := parseKeyValueDSN(got)
	if !ok {
		t.Fatalf("spliced DSN did not re-parse: %q", got)
	}
	var pw string
	var pwCount int
	for _, kv := range pairs {
		if kv.key == "password" {
			pw = kv.value
			pwCount++
		}
	}
	if pwCount != 1 || pw != "a b c" {
		t.Errorf("password round-trip broken: count=%d value=%q from %q", pwCount, pw, got)
	}
}

// TestSpliceDSNHostPort_UnterminatedQuoteFailsClosed: a malformed
// key-value DSN (unterminated quote) returns "" so the Coordinator
// surfaces dsn_build_failed rather than emitting a corrupt DSN.
func TestSpliceDSNHostPort_UnterminatedQuoteFailsClosed(t *testing.T) {
	if got := spliceDSNHostPort("host=old password='oops", "h", 5432); got != "" {
		t.Errorf("unterminated quote should fail closed; got %q", got)
	}
}

// --- Bug 66: parseSinceUntil / parseDurationWithDays accepts "7d" ---

// TestParseSinceUntil_DayForm is the #66 regression: the flag help
// advertises "7d" but time.ParseDuration rejects the day unit. Both
// the pure day form and combined forms must parse.
func TestParseSinceUntil_DayForm(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		in   string
		back time.Duration
	}{
		{"7d", 7 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
		{"24h", 24 * time.Hour},
		{"7d12h", 7*24*time.Hour + 12*time.Hour},
	} {
		got, err := parseSinceUntil(tc.in)
		if err != nil {
			t.Errorf("parseSinceUntil(%q) errored: %v", tc.in, err)
			continue
		}
		want := now.Add(-tc.back)
		if diff := got.Sub(want); diff < -2*time.Second || diff > 2*time.Second {
			t.Errorf("parseSinceUntil(%q) = %v, want ~%v (diff %v)", tc.in, got, want, diff)
		}
	}
}

// TestParseSinceUntil_RejectsGarbage: a value that's neither a duration
// (with or without days) nor an RFC3339 timestamp is still rejected.
func TestParseSinceUntil_RejectsGarbage(t *testing.T) {
	if _, err := parseSinceUntil("garbage"); err == nil {
		t.Error("expected error for garbage input")
	}
	if _, err := parseSinceUntil("7dgarbage"); err == nil {
		t.Error("expected error for a bad tail after the day component")
	}
}

// --- Bug 71: copyFile is atomic and self-copy safe ---

// TestCopyFile_SelfCopyDoesNotTruncate is the #71 regression: copying
// a file onto itself (re-installing a skill from its own installed
// path) must NOT truncate the source before reading it. The old
// O_TRUNC open zeroed the file first, losing all content.
func TestCopyFile_SelfCopyDoesNotTruncate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "skill.yaml")
	const content = "name: demo\nversion: 1\nbody: hello world\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(p, p); err != nil {
		t.Fatalf("copyFile self-copy: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("self-copy corrupted content: got %q want %q", got, content)
	}
}

// TestCopyFile_CopiesDistinctPath: the ordinary copy path still works.
func TestCopyFile_CopiesDistinctPath(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.yaml")
	dst := filepath.Join(dir, "dst.yaml")
	if err := os.WriteFile(src, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "abc" {
		t.Errorf("copy content = %q, want abc", got)
	}
	// A stale tmp from a prior crash must not wedge the re-copy.
	if err := os.WriteFile(dst+".tmp", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile over stale tmp: %v", err)
	}
}

// --- Bug 61: isManifestSignatureFailure distinguishes backend errors ---

// TestIsManifestSignatureFailure is the #61 classifier regression: a
// backend/storage error must NOT be treated as a signature failure
// (which would report "potential tampering" and present a truncated
// walk as complete), while genuine verification failures must.
func TestIsManifestSignatureFailure(t *testing.T) {
	// Backend errors → not a signature failure.
	for _, err := range []error{
		storage.ErrNotFound,
		storage.ErrChecksumMismatch,
		storage.ErrUnsupported,
		storage.ErrUnknownScheme,
	} {
		if isManifestSignatureFailure(err) {
			t.Errorf("%v should be classified as a backend error, not a signature failure", err)
		}
	}
	// Genuine verification failures → signature failure.
	for _, err := range []error{
		backup.ErrPublicKeyMismatch,
		backup.ErrUnsigned,
		backup.ErrBadSignature,
	} {
		if !isManifestSignatureFailure(err) {
			t.Errorf("%v should be classified as a signature failure", err)
		}
	}
	if isManifestSignatureFailure(nil) {
		t.Error("nil is not a failure")
	}
}

// --- Bug 64: doctor recomputes Healthy from the final issue set ---

// TestDoctorHasErrorOrHigher checks the threshold used to recompute
// Healthy after late-appended error/critical issues (#64).
func TestDoctorHasErrorOrHigher(t *testing.T) {
	if doctorHasErrorOrHigher(nil) {
		t.Error("empty issue set is not error-or-higher")
	}
	warnOnly := []doctorIssue{{Severity: output.SeverityWarning}}
	if doctorHasErrorOrHigher(warnOnly) {
		t.Error("warning-only must NOT flip Healthy to false")
	}
	withErr := []doctorIssue{{Severity: output.SeverityNotice}, {Severity: output.SeverityError}}
	if !doctorHasErrorOrHigher(withErr) {
		t.Error("an error-severity issue must flip Healthy to false")
	}
	withCrit := []doctorIssue{{Severity: output.SeverityCritical}}
	if !doctorHasErrorOrHigher(withCrit) {
		t.Error("a critical-severity issue must flip Healthy to false")
	}
}

// --- Bug 41: scrubManifestAware propagates a WAL-manifest List error ---

// walListFaultSP wraps a StoragePlugin and injects an error on the
// first List whose prefix starts with "wal/", so the test can prove
// scrubManifestAware surfaces (rather than swallows) a transient
// storage failure mid-walk.
type walListFaultSP struct {
	storage.StoragePlugin
}

func (w walListFaultSP) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	if strings.HasPrefix(prefix, "wal/") {
		return func(yield func(storage.ObjectInfo, error) bool) {
			yield(storage.ObjectInfo{}, errInjectedWALList)
		}
	}
	return w.StoragePlugin.List(ctx, prefix)
}

var errInjectedWALList = errors.New("injected: wal/ list failed")

// TestScrubManifestAware_PropagatesWALListError is the #41 regression:
// a transient List error during the WAL-manifest walk must NOT be
// silently broken out of (which reported a clean "no integrity
// failures"); it must propagate.
func TestScrubManifestAware_PropagatesWALListError(t *testing.T) {
	ctx := context.Background()
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(ctx, storage.StorageConfig{URL: mustURL(t, repoURL)}); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sp.Close()

	// Point the CLI's keyring at a temp dir so loadVerifier succeeds.
	setTestKeyringHome(t)

	faulty := walListFaultSP{StoragePlugin: sp}
	_, _, err := scrubManifestAware(ctx, faulty, 0)
	if err == nil {
		t.Fatal("expected the injected wal/ List error to propagate, got nil")
	}
	if !errors.Is(err, errInjectedWALList) {
		t.Errorf("expected errInjectedWALList, got %v", err)
	}
}
