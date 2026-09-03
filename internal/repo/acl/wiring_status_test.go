package acl_test

// The package doc says this ACL boundary is NOT wired into anything.
// This test is what keeps that true — or forces the doc to change.
//
// The doc used to claim the opposite: that it "closes" the SPEC
// commitment for cross-account replication, and that "before any
// byte-copy, both policies are loaded and verified". Neither was so.
// SPEC.md lists the feature as Planned, `repo replicate` copies bytes
// without consulting either policy, and no production file imports this
// package at all. A reader of that doc would have concluded replication
// is gated today.
//
// That is the same defect this audit kept finding elsewhere, in its
// purest form: a claim nothing checks, drifting from the code it
// describes. So the claim is checked.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const aclImportPath = "github.com/cybertec-postgresql/pg_hardstorage/internal/repo/acl"

func TestACL_IsNotWiredIntoAnyProductionPath(t *testing.T) {
	root := "../../.." // repo root, from internal/repo/acl
	var importers []string

	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable trees are not this test's business
		}
		if info.IsDir() {
			base := filepath.Base(p)
			if base == ".git" || base == "test-runs" || base == "vendor" || base == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		// Skip the package's own files.
		if strings.Contains(filepath.ToSlash(p), "/internal/repo/acl/") {
			return nil
		}
		body, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		if strings.Contains(string(body), aclImportPath) {
			importers = append(importers, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(importers) > 0 {
		t.Fatalf("internal/repo/acl is now imported by production code:\n  %s\n\n"+
			"That is a good thing — but two things must happen with it:\n\n"+
			"  1. Update this package's doc comment. It states \"STATUS: NOT WIRED\" and "+
			"says repo replicate copies bytes without consulting either policy. Leaving "+
			"that in place recreates exactly the drift this test exists to prevent, and "+
			"SPEC.md's \"Planned\" entry needs revisiting too.\n\n"+
			"  2. Harden the policy readers first. LoadSource/LoadAccept parse with "+
			"encoding/json and the signature is checked against bytes RE-CANONICALISED "+
			"from the struct, so a policy carrying a duplicate JSON key verifies while a "+
			"first-wins reader sees a different grant. The backup manifest had this exact "+
			"hole; see backup.rejectDuplicateKeys. An ACL grant decides where data may be "+
			"copied — it must not be ambiguous.\n\n"+
			"Then delete or rewrite this test.", strings.Join(importers, "\n  "))
	}
}
