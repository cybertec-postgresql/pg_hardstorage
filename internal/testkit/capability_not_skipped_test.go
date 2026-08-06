package testkit_test

// capability_not_skipped_test.go — a backend that lacks a capability
// must have its DEGRADED behaviour asserted, never skipped.
//
// Sibling of soak_not_vacuous_test.go. That one catches a test that
// skips because an env var is unset; this one catches a test that skips
// because the backend cannot do something. The failure mode is the
// same and so is the reason it matters: `go test` prints ok for a
// skipped test, and a fixture that quietly cannot do the thing under
// test is indistinguishable from a product that does it correctly.
//
// The rule is not "never skip". It is: a capability the plugin
// ADVERTISES is a fact about the product, so branching on it must lead
// to an assertion about what the product does instead. The pattern to
// copy is internal/backup/lease_backends_integration_test.go:
//
//	if !sp.Capabilities().ConditionalPut {
//	    // must refuse rather than hand out a lock that locks nothing
//	    _, err := AcquireBackupLease(...)
//	    if !errors.Is(err, ErrLeaseNotEnforceable) { t.Fatalf(...) }
//	    return
//	}
//
// That asserts the contract for the weak backend. Skipping there would
// have hidden exactly the bug that made leases unenforceable on
// stat-then-write backends.
//
// This is a source-shape check because the failure mode is a test that
// does nothing — which no behavioural assertion can tell apart from
// success.
//
// Concrete cost of getting this wrong, from the 2026-08-05 campaign: a
// WORM probe reported the product as not enforcing retention. It was
// the fixture — a MinIO bucket created without Object Lock. Four of the
// seven "bugs" that campaign produced were fixture limits read as
// product defects.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// capabilityGatedSkip matches a Capabilities() read followed closely by
// a t.Skip — the shape that turns "this backend can't" into a green
// tick. The window allows braces and a comment line, because the real
// shape is:
//
//	if !sp.Capabilities().ConditionalPut {
//	    t.Skip("backend cannot do conditional put")
//	}
var capabilityGatedSkip = regexp.MustCompile(
	`(?s)Capabilities\(\)\.([A-Za-z]+)[\s\S]{0,200}?t\.Skip`)

// coverageReference is the trail a legitimate skip leaves: a pointer to
// the test that DOES assert what the weak backend must do.
var coverageReference = regexp.MustCompile(
	`(?i)(covered by|asserted (in|by)|proven by|see [a-z_/.]*test)`)

func TestCapabilityGapsAreAssertedNotSkipped(t *testing.T) {
	root := repoRoot(t)

	var found []string
	scanned := 0
	err := filepath.WalkDir(filepath.Join(root, "internal"),
		func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// This file quotes the forbidden shape in its own comment.
			if strings.HasSuffix(path, "capability_not_skipped_test.go") {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			scanned++
			text := string(src)
			for _, m := range capabilityGatedSkip.FindAllStringSubmatchIndex(text, -1) {
				// A skip IS acceptable when it says where the degraded
				// contract is covered instead. Two of the four sites
				// this guard first flagged did exactly that
				// ("covered by the exclusion test") and were correct;
				// failing them would have been the guard calling good
				// tests wrong. What is not acceptable is a skip that
				// leaves no trail, because then nobody can tell whether
				// the weak backend is tested anywhere at all.
				tail := text[m[1]:min(len(text), m[1]+220)]
				if coverageReference.MatchString(tail) {
					continue
				}
				capName := text[m[2]:m[3]]
				line := strings.Count(text[:m[0]], "\n") + 1
				rel, _ := filepath.Rel(root, path)
				found = append(found, filepath.ToSlash(rel)+":"+strconv.Itoa(line)+
					"  skips on Capabilities()."+capName+" without saying where the "+
					"degraded behaviour is asserted")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	if scanned == 0 {
		t.Fatal("scanned no test files — the walk broke and this guard asserts nothing")
	}
	t.Logf("scanned %d test files for capability-gated skips", scanned)

	if len(found) > 0 {
		sort.Strings(found)
		t.Errorf("%d test(s) skip when a backend lacks a capability, with no pointer to "+
			"where that backend's behaviour IS asserted:\n  %s\n\n"+
			"Three options, in order of preference: assert what the product does without "+
			"the capability; t.Fatal because the fixture cannot prove anything (see "+
			"internal/plugin/storage/commit_test.go — \"fixture does not advertise "+
			"ConditionalPut; this test would prove nothing\"); or skip and say where it is "+
			"covered. See "+
			"internal/backup/lease_backends_integration_test.go: a backend without "+
			"ConditionalPut must refuse the lease with ErrLeaseNotEnforceable, and that "+
			"assertion is what caught leases silently locking nothing on stat-then-write "+
			"backends. A skip there would have reported ok.",
			len(found), strings.Join(found, "\n  "))
	}
}

// TestCapabilityGuardCanActuallyFail proves the regex matches the shape
// it targets. An earlier guard in this package shipped with a pattern
// that excluded `{` and therefore matched nothing — it passed on the
// very code it was written to catch. A guard that cannot fail is worse
// than none, so this one is shown a synthetic violation.
func TestCapabilityGuardCanActuallyFail(t *testing.T) {
	violation := `
	if !sp.Capabilities().ConditionalPut {
		t.Skip("backend cannot do conditional put")
	}`
	if !capabilityGatedSkip.MatchString(violation) {
		t.Fatal("capabilityGatedSkip does not match the shape it exists to catch — the " +
			"guard above is passing vacuously")
	}

	clean := `
	if !sp.Capabilities().ConditionalPut {
		_, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{})
		if !errors.Is(err, ErrLeaseNotEnforceable) {
			t.Fatalf("want refusal, got %v", err)
		}
		return
	}`
	if capabilityGatedSkip.MatchString(clean) {
		t.Error("capabilityGatedSkip matches the CORRECT shape (assert the degraded " +
			"behaviour), so it would flag good tests")
	}
}
