package audit_test

// append_not_silent_test.go — an audit append may fail, but it may not
// fail silently.
//
// Store.Append returns an error, and a failure means the chain has a
// gap. AppendOrLog exists precisely so that gap is reported:
//
//	audit: append %q failed (chain may have a gap): %v
//
// Ten call sites wrote `_ = auditStore.Append(...)` instead, discarding
// that error with nothing logged. The actions they record are the ones
// an auditor comes looking for — jit.issue and jit.revoke (privileged
// access granted and withdrawn), hold (legal hold placed or lifted),
// threshold.attest_sign and threshold.roster_create, dsa.locate (a GDPR
// subject-access lookup), insider.scan, integrity.run, and
// wal.gap_purged.
//
// That last one is the sharpest: `wal gap purge` removes the record of
// a WAL gap. If its audit append is dropped, the evidence of a gap is
// gone AND there is no trace that anyone removed it.
//
// Best-effort is the right POSTURE — an audit failure must not fail the
// operation the operator asked for, since refusing to purge because we
// could not write a log entry helps nobody. What is not acceptable is
// silence. AppendOrLog gives both.
//
// A source-shape check, because the failure mode is an error that goes
// nowhere: no behavioural assertion can see a log line that was never
// written.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// silentAppend matches an append whose error is discarded, on ANY
// receiver.
//
// Scoping is by FILE rather than by receiver name: only files that
// import internal/audit are scanned, so an unrelated Append (a strings
// builder, a slice helper) is never considered. The first version keyed
// on a receiver matching `*[Ss]tore`, which would have missed
// `_ = s.Append(ctx, ev)` with a short receiver — the same blind spot
// as the soak guard whose pattern could not express a variable name
// containing a digit. The self-test below is what surfaced it.
var silentAppend = regexp.MustCompile(`_\s*=\s*[\w.]+\.Append\(`)

// importsAudit reports whether a file uses the audit package, which is
// what makes an `.Append(` in it an AUDIT append.
func importsAudit(src string) bool {
	return strings.Contains(src, `"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"`)
}

func repoRootFromAudit(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
}

func TestAuditAppendsAreNeverSilent(t *testing.T) {
	root := repoRootFromAudit(t)

	var offenders []string
	scanned := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", ".git", "test-runs", "node_modules", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		// audit.go documents the forbidden shape in AppendOrLog's own
		// comment, which is where the alternative is explained.
		if rel == "internal/audit/audit.go" {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if !importsAudit(string(src)) {
			return nil
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			if silentAppend.MatchString(line) {
				offenders = append(offenders, rel+":"+strconv.Itoa(i+1)+"  "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned == 0 {
		t.Fatal("scanned no production Go files — the walk broke and this guard asserts nothing")
	}
	t.Logf("scanned %d production file(s) for silent audit appends", scanned)

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d audit append(s) discard their error:\n  %s\n\n"+
			"Use AppendOrLog. Best-effort is the right posture — an audit failure must not "+
			"fail the operation the operator asked for — but the failure has to be visible: "+
			"a dropped append means the chain has a gap and nothing said so. For "+
			"`wal gap purge` that means the evidence of a WAL gap is gone and there is no "+
			"record that anyone removed it.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// TestAuditAppendGuardCanFail proves the pattern matches the shape it
// targets. A guard whose regex cannot express the code it polices
// passes for the wrong reason — this repository has shipped two of
// those, one of which could not match a variable name containing a
// digit.
func TestAuditAppendGuardCanFail(t *testing.T) {
	for _, shape := range []string{
		`_ = auditStore.Append(cmd.Context(), ev)`,
		`_ = store.Append(ctx, &audit.Event{})`,
		`	_  =  s.Append(ctx, ev)`,
	} {
		if !silentAppend.MatchString(shape) {
			t.Errorf("silentAppend does not match %q — the guard above is passing vacuously", shape)
		}
	}
	for _, ok := range []string{
		`auditStore.AppendOrLog(cmd.Context(), ev)`,
		`if err := store.Append(ctx, ev); err != nil {`,
		`err = store.Append(ctx, ev)`,
		`sb.WriteString("x")`,
	} {
		if silentAppend.MatchString(ok) {
			t.Errorf("silentAppend matches the CORRECT shape %q, so it would flag good code", ok)
		}
	}
}
