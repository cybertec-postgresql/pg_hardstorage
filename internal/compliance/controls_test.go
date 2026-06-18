package compliance_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/compliance"
)

// TestAssessControls_NilReport returns nil — defensive guard.
func TestAssessControls_NilReport(t *testing.T) {
	if compliance.AssessControls(nil) != nil {
		t.Errorf("expected nil")
	}
}

// TestAssessControls_AllNotApplicable_WhenSectionsAbsent: an
// empty report (every section nil) must produce all-N/A controls.
// No fail verdicts on a "minimal" report.
func TestAssessControls_AllNotApplicable_WhenSectionsAbsent(t *testing.T) {
	r := &compliance.Report{Schema: "x"}
	cs := compliance.AssessControls(r)
	if cs == nil {
		t.Fatal("nil ControlSection")
	}
	if cs.Total == 0 {
		t.Fatal("expected some controls")
	}
	if cs.Pass != 0 || cs.Fail != 0 {
		t.Errorf("Pass=%d Fail=%d on empty report; want all not_applicable",
			cs.Pass, cs.Fail)
	}
	if cs.NotApplicable != cs.Total {
		t.Errorf("NotApplicable=%d, want %d", cs.NotApplicable, cs.Total)
	}
}

// TestAssessControls_Encryption_PassFail covers the encryption
// branch: 100% coverage → pass, anything less → fail with
// remediation.
func TestAssessControls_Encryption_PassFail(t *testing.T) {
	pass := &compliance.Report{
		Encryption: &compliance.EncryptionSection{
			EncryptedCount:   10,
			UnencryptedCount: 0,
			CoveragePercent:  100.0,
		},
	}
	cs := compliance.AssessControls(pass)
	for _, c := range cs.Controls {
		if c.Section == "encryption" && c.Status != compliance.StatusPass {
			t.Errorf("encryption pass expected; got %s for %s/%s",
				c.Status, c.Framework, c.ControlID)
		}
	}

	fail := &compliance.Report{
		Encryption: &compliance.EncryptionSection{
			EncryptedCount:   8,
			UnencryptedCount: 2,
			CoveragePercent:  80.0,
		},
	}
	cs = compliance.AssessControls(fail)
	encFails := 0
	for _, c := range cs.Controls {
		if c.Section != "encryption" {
			continue
		}
		if c.Status != compliance.StatusFail {
			t.Errorf("encryption fail expected; got %s", c.Status)
		}
		if c.Remediation == "" {
			t.Errorf("expected non-empty remediation on failed encryption control")
		}
		encFails++
	}
	if encFails == 0 {
		t.Errorf("no encryption controls in the assessment")
	}
}

// TestAssessControls_Verification: zero-runs in window is a fail.
func TestAssessControls_Verification_ZeroRunsFails(t *testing.T) {
	r := &compliance.Report{
		Verification: &compliance.VerificationSection{TotalRuns: 0},
	}
	cs := compliance.AssessControls(r)
	for _, c := range cs.Controls {
		if c.Section != "verification" {
			continue
		}
		if c.Status != compliance.StatusFail {
			t.Errorf("expected fail on zero verification runs; got %s", c.Status)
		}
	}
}

// TestAssessControls_Chain: VerifyOK + zero mismatches → pass;
// VerifyOK=false → fail.
func TestAssessControls_Chain_OKFails(t *testing.T) {
	pass := &compliance.Report{
		Chain: &compliance.ChainSection{
			VerifyOK:             true,
			VerifyEventsChecked:  100,
			VerifyHashMismatches: 0,
			VerifyChainBreaks:    0,
		},
	}
	cs := compliance.AssessControls(pass)
	for _, c := range cs.Controls {
		if c.Section != "chain" {
			continue
		}
		if c.Status != compliance.StatusPass {
			t.Errorf("chain pass expected; got %s", c.Status)
		}
	}

	fail := &compliance.Report{
		Chain: &compliance.ChainSection{
			VerifyOK:             false,
			VerifyHashMismatches: 1,
		},
	}
	cs = compliance.AssessControls(fail)
	for _, c := range cs.Controls {
		if c.Section != "chain" {
			continue
		}
		if c.Status != compliance.StatusFail {
			t.Errorf("chain fail expected; got %s", c.Status)
		}
	}
}

// TestAssessControls_WORM: configured + active → pass; configured
// but inactive → fail; absent → not_applicable.
func TestAssessControls_WORM(t *testing.T) {
	cases := []struct {
		name string
		s    *compliance.WORMSection
		want compliance.ControlStatus
	}{
		{"absent", nil, compliance.StatusNotApplicable},
		{"empty mode", &compliance.WORMSection{Mode: ""}, compliance.StatusNotApplicable},
		{"configured + active", &compliance.WORMSection{
			Mode: "compliance", Retention: "7y", Active: true,
		}, compliance.StatusPass},
		{"configured but inactive", &compliance.WORMSection{
			Mode: "compliance", Retention: "7y", Active: false,
		}, compliance.StatusFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &compliance.Report{WORM: tc.s}
			cs := compliance.AssessControls(r)
			for _, c := range cs.Controls {
				if c.Section != "worm" {
					continue
				}
				if c.Status != tc.want {
					t.Errorf("WORM(%v) → %s, want %s", tc.s, c.Status, tc.want)
				}
			}
		})
	}
}

// TestAssessControls_Approvals: zero destructive ops → pass
// (no scope); destructive ops with zero approval requests → fail.
func TestAssessControls_Approvals(t *testing.T) {
	noOps := &compliance.Report{
		Approvals: &compliance.ApprovalSection{DestructiveOps: 0},
	}
	cs := compliance.AssessControls(noOps)
	for _, c := range cs.Controls {
		if c.Section == "approvals" && c.Status != compliance.StatusPass {
			t.Errorf("zero ops should pass; got %s", c.Status)
		}
	}

	unapproved := &compliance.Report{
		Approvals: &compliance.ApprovalSection{
			DestructiveOps:  3,
			RequestsCreated: 0,
		},
	}
	cs = compliance.AssessControls(unapproved)
	for _, c := range cs.Controls {
		if c.Section == "approvals" && c.Status != compliance.StatusFail {
			t.Errorf("unapproved destructive ops should fail; got %s", c.Status)
		}
	}
}

// TestAssessControls_Replicas: zero unreplicated → pass; some
// unreplicated → fail.
func TestAssessControls_Replicas(t *testing.T) {
	pass := &compliance.Report{
		Replicas: &compliance.ReplicaSection{
			WindowedPrimaries:     5,
			WindowedReplicaCopies: 5,
			UnreplicatedInWindow:  0,
		},
	}
	cs := compliance.AssessControls(pass)
	for _, c := range cs.Controls {
		if c.Section == "replicas" && c.Status != compliance.StatusPass {
			t.Errorf("fully-replicated should pass; got %s", c.Status)
		}
	}

	fail := &compliance.Report{
		Replicas: &compliance.ReplicaSection{
			WindowedPrimaries:     5,
			WindowedReplicaCopies: 3,
			UnreplicatedInWindow:  2,
		},
	}
	cs = compliance.AssessControls(fail)
	for _, c := range cs.Controls {
		if c.Section == "replicas" && c.Status != compliance.StatusFail {
			t.Errorf("partial-replication should fail; got %s", c.Status)
		}
	}
}

// TestAssessControls_FrameworksAllPresent: every Framework
// constant must appear at least once in the produced controls
// table.  Asserts the mapping is exhaustive — adding a framework
// without wiring controls would silently drop coverage.
func TestAssessControls_FrameworksAllPresent(t *testing.T) {
	r := &compliance.Report{Schema: "x"}
	cs := compliance.AssessControls(r)
	want := map[compliance.Framework]bool{
		compliance.FrameworkSOC2:     false,
		compliance.FrameworkISO27001: false,
		compliance.FrameworkHIPAA:    false,
		compliance.FrameworkPCIDSS:   false,
		compliance.FrameworkFedRAMP:  false,
	}
	for _, c := range cs.Controls {
		if _, ok := want[c.Framework]; ok {
			want[c.Framework] = true
		}
	}
	for fw, present := range want {
		if !present {
			t.Errorf("framework %q has no controls in the assessment", fw)
		}
	}
}

// TestAssessControls_CountsAddUp: Pass + Fail + NotApplicable
// must equal Total.
func TestAssessControls_CountsAddUp(t *testing.T) {
	r := &compliance.Report{
		Encryption: &compliance.EncryptionSection{
			EncryptedCount: 10, CoveragePercent: 100.0,
		},
		Verification: &compliance.VerificationSection{TotalRuns: 5},
		Chain: &compliance.ChainSection{
			VerifyOK: true, VerifyEventsChecked: 100,
		},
	}
	cs := compliance.AssessControls(r)
	if cs.Pass+cs.Fail+cs.NotApplicable != cs.Total {
		t.Errorf("counts don't add up: %d + %d + %d != %d",
			cs.Pass, cs.Fail, cs.NotApplicable, cs.Total)
	}
}

// TestAssessControls_RemediationOnlyOnFails: pass + N/A controls
// have empty Remediation; only fails carry it.
func TestAssessControls_RemediationOnlyOnFails(t *testing.T) {
	r := &compliance.Report{
		Encryption: &compliance.EncryptionSection{
			EncryptedCount: 5, UnencryptedCount: 5, CoveragePercent: 50.0,
		},
		Verification: &compliance.VerificationSection{TotalRuns: 5},
	}
	cs := compliance.AssessControls(r)
	for _, c := range cs.Controls {
		switch c.Status {
		case compliance.StatusPass, compliance.StatusNotApplicable:
			if c.Remediation != "" {
				t.Errorf("%s control %s/%s carries remediation: %q",
					c.Status, c.Framework, c.ControlID, c.Remediation)
			}
		case compliance.StatusFail:
			if c.Remediation == "" {
				t.Errorf("failed control %s/%s has empty remediation",
					c.Framework, c.ControlID)
			}
		}
	}
}

// TestAssessControls_EvidenceAlwaysPopulated: every control has
// a non-empty Evidence field for forensics.
func TestAssessControls_EvidenceAlwaysPopulated(t *testing.T) {
	r := &compliance.Report{Schema: "x"}
	cs := compliance.AssessControls(r)
	for _, c := range cs.Controls {
		if strings.TrimSpace(c.Evidence) == "" {
			t.Errorf("control %s/%s has empty Evidence", c.Framework, c.ControlID)
		}
	}
}
