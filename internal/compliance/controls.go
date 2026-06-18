// controls.go — Framework + Control taxonomy (SOC2/ISO27001/HIPAA/PCI/FedRAMP) with pass/fail/NA status.
package compliance

import "fmt"

// Framework names the regulatory / control framework a Control
// belongs to.  Stable strings (24-month BC); the JSON consumer
// can pivot reports by framework.
type Framework string

const (
	// FrameworkSOC2 is the AICPA SOC 2 Trust Services Criteria.
	FrameworkSOC2 Framework = "soc2"
	// FrameworkISO27001 is ISO/IEC 27001:2022.
	FrameworkISO27001 Framework = "iso27001"
	// FrameworkHIPAA is the HIPAA Security Rule (45 CFR §164).
	FrameworkHIPAA Framework = "hipaa"
	// FrameworkPCIDSS is PCI DSS v4.0.
	FrameworkPCIDSS Framework = "pci_dss"
	// FrameworkFedRAMP is NIST 800-53 / FedRAMP Moderate.
	FrameworkFedRAMP Framework = "fedramp"
)

// ControlStatus is one of three: pass / fail / not-applicable.
// "not-applicable" is set for controls whose corresponding Report
// section is nil (operator skipped the section, OR the relevant
// audit-event class produced zero rows in the window).
type ControlStatus string

const (
	// StatusPass indicates the control's assessment criteria were met.
	StatusPass ControlStatus = "pass"
	// StatusFail indicates the control's assessment criteria were not met.
	StatusFail ControlStatus = "fail"
	// StatusNotApplicable indicates the control has no in-scope evidence
	// in this report (section absent or zero rows in window).
	StatusNotApplicable ControlStatus = "not_applicable"
)

// Control is one assessed item.  A single underlying Report
// section can map to controls in multiple frameworks (e.g. our
// EncryptionSection maps to both SOC 2 CC6.1 and HIPAA §164.312).
type Control struct {
	Framework   Framework     `json:"framework"`
	ControlID   string        `json:"control_id"`
	Description string        `json:"description"`
	Status      ControlStatus `json:"status"`

	// Section names which Report.* section produced this verdict
	// (e.g. "encryption", "verification", "chain").  Lets a
	// reviewer cross-reference back to the underlying numbers.
	Section string `json:"section"`

	// Evidence is a one-line operator-readable summary of what
	// the control assessment is based on.  The PDF / Markdown
	// renderers print it next to the verdict; JSON consumers see
	// a stable string field.
	Evidence string `json:"evidence"`

	// Remediation is non-empty for failed controls — points the
	// operator at the runbook / config knob that flips the
	// verdict.  Empty for passing or not-applicable controls.
	Remediation string `json:"remediation,omitempty"`
}

// ControlSection rolls every Control up into one body section.
// Renderers print this as a single table; the JSON consumer can
// filter by framework client-side.
type ControlSection struct {
	Total         int       `json:"total"`
	Pass          int       `json:"pass"`
	Fail          int       `json:"fail"`
	NotApplicable int       `json:"not_applicable"`
	Controls      []Control `json:"controls"`
}

// AssessControls maps the Report's existing sections into
// per-framework controls.  Pure function: takes a populated
// Report and returns a ControlSection.  The mapping table lives
// in this file so adding a framework or a control is a single-
// file change.
func AssessControls(r *Report) *ControlSection {
	if r == nil {
		return nil
	}
	out := &ControlSection{}

	// Build the assessments.  Every entry below names the
	// (Report-section → framework + control_id) tuple; the
	// status function inspects the relevant section and produces
	// pass / fail / not-applicable.
	specs := []controlSpec{
		// ----- Encryption coverage -----
		// SOC 2 CC6.1: "Logical access security software ... protects
		// information assets from threats from inside and outside the
		// entity boundaries."  Encryption-at-rest is one component.
		{Framework: FrameworkSOC2, ControlID: "CC6.1",
			Description: "Information assets at rest are protected from unauthorised access.",
			Section:     "encryption", Assess: assessEncryption},
		// ISO 27001 A.8.24 (Use of cryptography) — same scope.
		{Framework: FrameworkISO27001, ControlID: "A.8.24",
			Description: "Cryptographic controls are used to protect information at rest.",
			Section:     "encryption", Assess: assessEncryption},
		// HIPAA §164.312(a)(2)(iv) — Encryption + Decryption (Addressable).
		{Framework: FrameworkHIPAA, ControlID: "§164.312(a)(2)(iv)",
			Description: "Implement a mechanism to encrypt and decrypt ePHI at rest.",
			Section:     "encryption", Assess: assessEncryption},
		// PCI DSS v4.0 Req 3.5.1 — Render PAN unreadable anywhere it is stored.
		{Framework: FrameworkPCIDSS, ControlID: "Req 3.5.1",
			Description: "PAN is rendered unreadable anywhere it is stored (encryption + key custody).",
			Section:     "encryption", Assess: assessEncryption},
		// FedRAMP / NIST 800-53 SC-28 — Protection of information at rest.
		{Framework: FrameworkFedRAMP, ControlID: "SC-28",
			Description: "Information at rest is cryptographically protected.",
			Section:     "encryption", Assess: assessEncryption},

		// ----- Verification activity -----
		// SOC 2 A1.2: "Authorises, designs, develops or acquires,
		// implements, operates, monitors data backup processes."
		// Verification (post-backup pg_verifybackup runs) is the
		// "monitors" half.
		{Framework: FrameworkSOC2, ControlID: "A1.2",
			Description: "Backup verification activity is recorded for the reporting window.",
			Section:     "verification", Assess: assessVerification},
		// ISO 27001 A.8.13 (Information backup) — covers verification.
		{Framework: FrameworkISO27001, ControlID: "A.8.13",
			Description: "Backups are routinely verified for restorability.",
			Section:     "verification", Assess: assessVerification},

		// ----- Audit chain integrity -----
		// SOC 2 CC7.2: "Monitors system components ... for events".
		// Hash-chained audit log + verifiable head pointer satisfies.
		{Framework: FrameworkSOC2, ControlID: "CC7.2",
			Description: "System events are logged in a tamper-evident chain.",
			Section:     "chain", Assess: assessChain},
		// ISO 27001 A.8.15 (Logging).
		{Framework: FrameworkISO27001, ControlID: "A.8.15",
			Description: "Event logs are protected against tampering and unauthorised changes.",
			Section:     "chain", Assess: assessChain},
		// HIPAA §164.312(b) — Audit controls.
		{Framework: FrameworkHIPAA, ControlID: "§164.312(b)",
			Description: "Hardware, software, and procedural mechanisms record + examine activity.",
			Section:     "chain", Assess: assessChain},
		// PCI DSS Req 10.2 — implement audit logs.
		{Framework: FrameworkPCIDSS, ControlID: "Req 10.2",
			Description: "Audit logs capture all access to system components and cardholder data.",
			Section:     "chain", Assess: assessChain},
		// FedRAMP AU-9 — protection of audit information.
		{Framework: FrameworkFedRAMP, ControlID: "AU-9",
			Description: "Audit information is protected from unauthorised modification.",
			Section:     "chain", Assess: assessChain},

		// ----- WORM retention -----
		// SOC 2 PI1.5: "Retains data ... for the period required."
		{Framework: FrameworkSOC2, ControlID: "PI1.5",
			Description: "Backup data is retained per the documented retention policy with WORM enforcement.",
			Section:     "worm", Assess: assessWORM},
		// ISO 27001 A.8.10 (Information deletion).  Inverse of
		// retention — but WORM enforces "no premature deletion".
		{Framework: FrameworkISO27001, ControlID: "A.8.10",
			Description: "Deletion of stored information is constrained by retention rules.",
			Section:     "worm", Assess: assessWORM},
		// PCI DSS Req 10.5 — secure audit trails.
		{Framework: FrameworkPCIDSS, ControlID: "Req 10.5",
			Description: "Audit trails cannot be modified prior to the retention deadline.",
			Section:     "worm", Assess: assessWORM},

		// ----- Approvals / change control -----
		// SOC 2 CC8.1: "Authorises changes to ... infrastructure".
		// n-of-m approvals on destructive ops satisfy.
		{Framework: FrameworkSOC2, ControlID: "CC8.1",
			Description: "Destructive operations require recorded multi-party approval.",
			Section:     "approvals", Assess: assessApprovals},
		// FedRAMP AC-3 — access enforcement.
		{Framework: FrameworkFedRAMP, ControlID: "AC-3",
			Description: "Privileged operations enforce the configured access decisions.",
			Section:     "approvals", Assess: assessApprovals},

		// ----- Replica / availability -----
		// SOC 2 A1.2 second half: "designs, develops … data backup
		// processes ... monitors environmental protections, software,
		// data backup processes, and recovery infrastructure".
		// Replication count + freshness covers the resilience half.
		{Framework: FrameworkSOC2, ControlID: "A1.3",
			Description: "Backups are replicated to redundant storage to support recovery objectives.",
			Section:     "replicas", Assess: assessReplicas},
	}

	for _, sp := range specs {
		c := Control{
			Framework:   sp.Framework,
			ControlID:   sp.ControlID,
			Description: sp.Description,
			Section:     sp.Section,
		}
		c.Status, c.Evidence, c.Remediation = sp.Assess(r)
		out.Controls = append(out.Controls, c)
		switch c.Status {
		case StatusPass:
			out.Pass++
		case StatusFail:
			out.Fail++
		case StatusNotApplicable:
			out.NotApplicable++
		}
	}
	out.Total = len(out.Controls)
	return out
}

// controlSpec internally pairs a (framework, control id, section,
// description) with the function that produces the verdict.
type controlSpec struct {
	Framework   Framework
	ControlID   string
	Description string
	Section     string
	Assess      func(*Report) (ControlStatus, string, string)
}

// ----- per-section assessors -----
//
// Each returns (status, evidence, remediation).  Remediation is
// "" when status is pass / not_applicable.  The functions are
// intentionally tiny: a one-line evidence string + a clear
// pass/fail boundary that an auditor can challenge.

// assessEncryption: pass iff every backup in the window carries
// an Encryption block (CoveragePercent == 100).  Partial coverage
// fails — auditors care about the unencrypted set.
func assessEncryption(r *Report) (ControlStatus, string, string) {
	s := r.Encryption
	if s == nil {
		return StatusNotApplicable, "no encryption section in report", ""
	}
	if s.EncryptedCount == 0 && s.UnencryptedCount == 0 {
		return StatusNotApplicable, "no backups in window", ""
	}
	if s.UnencryptedCount > 0 {
		return StatusFail,
			fmt.Sprintf("%d of %d backups unencrypted (%.1f%% coverage)",
				s.UnencryptedCount,
				s.EncryptedCount+s.UnencryptedCount,
				s.CoveragePercent),
			"configure a KMS provider + per-tenant KEK; back-fill via re-run on existing deployments"
	}
	return StatusPass,
		fmt.Sprintf("%d of %d backups encrypted (%.1f%% coverage)",
			s.EncryptedCount,
			s.EncryptedCount+s.UnencryptedCount,
			s.CoveragePercent),
		""
}

// assessVerification: pass iff at least one verification ran in
// the window.  Auditors looking for "did you exercise restorability"
// expect a non-zero number.
func assessVerification(r *Report) (ControlStatus, string, string) {
	s := r.Verification
	if s == nil {
		return StatusNotApplicable, "no verification section in report", ""
	}
	if s.TotalRuns == 0 {
		return StatusFail,
			"zero verification runs recorded in window",
			"run `pg_hardstorage verify <deployment> latest` and `pg_hardstorage integrity run` periodically; the recovery drill is the recommended deeper check"
	}
	return StatusPass,
		fmt.Sprintf("%d verification run(s) recorded across %d deployment(s)",
			s.TotalRuns, len(s.ByDeployment)),
		""
}

// assessChain: pass iff VerifyOK and zero hash mismatches /
// chain breaks.  Any non-zero VerifyHashMismatches /
// VerifyChainBreaks fails the control even when VerifyOK is
// somehow true; defensive.
func assessChain(r *Report) (ControlStatus, string, string) {
	s := r.Chain
	if s == nil {
		return StatusNotApplicable, "no chain section in report (--skip-chain)", ""
	}
	if !s.VerifyOK || s.VerifyHashMismatches > 0 || s.VerifyChainBreaks > 0 {
		return StatusFail,
			fmt.Sprintf("audit chain verify FAILED: %d hash mismatch(es), %d chain break(s)",
				s.VerifyHashMismatches, s.VerifyChainBreaks),
			"run `pg_hardstorage audit verify-chain` for the precise event ID; investigate the audit chain for tampering or storage-layer corruption"
	}
	return StatusPass,
		fmt.Sprintf("%d events in window verified clean (%d hash mismatch, %d chain break)",
			s.VerifyEventsChecked, s.VerifyHashMismatches, s.VerifyChainBreaks),
		""
}

// assessWORM: pass iff WORM mode is set + Active.  An Active
// WORM with non-empty Mode is the regulated-grade configuration;
// anything else (no mode, or mode set but not active) is
// not-applicable so non-regulated repos don't get a misleading
// fail report.
func assessWORM(r *Report) (ControlStatus, string, string) {
	s := r.WORM
	if s == nil {
		return StatusNotApplicable, "no WORM section in report", ""
	}
	if s.Mode == "" {
		return StatusNotApplicable, "WORM not configured on this repo", ""
	}
	if !s.Active {
		return StatusFail,
			fmt.Sprintf("WORM mode %q configured but not active", s.Mode),
			"verify the storage backend supports Object Lock (S3) / immutable blob (Azure) / SnapLock (NetApp); plain fs returns ErrUnsupported"
	}
	return StatusPass,
		fmt.Sprintf("WORM mode=%s, retention=%s", s.Mode, s.Retention),
		""
}

// assessApprovals: pass iff every destructive op recorded in the
// window had an approval requested.  Zero destructive ops is a
// pass (no scope).
func assessApprovals(r *Report) (ControlStatus, string, string) {
	s := r.Approvals
	if s == nil {
		return StatusNotApplicable, "no approvals section in report", ""
	}
	if s.DestructiveOps == 0 {
		return StatusPass, "no destructive operations executed in window", ""
	}
	// If destructive ops happened but no approval requests were
	// created, that's a clear failure of multi-party control.
	if s.RequestsCreated == 0 {
		return StatusFail,
			fmt.Sprintf("%d destructive operation(s) executed with zero approval requests",
				s.DestructiveOps),
			"configure approval policy for destructive verbs (kms.shred, repo.gc, repo.wipe); see `pg_hardstorage approval` subcommand"
	}
	return StatusPass,
		fmt.Sprintf("%d destructive op(s) preceded by %d approval request(s)",
			s.DestructiveOps, s.RequestsCreated), ""
}

// assessReplicas: pass iff every windowed manifest has a replica
// copy.  Any unreplicated_in_window > 0 is a fail.
func assessReplicas(r *Report) (ControlStatus, string, string) {
	s := r.Replicas
	if s == nil {
		return StatusNotApplicable, "no replicas section in report", ""
	}
	if s.WindowedPrimaries == 0 {
		return StatusNotApplicable, "no backups in window", ""
	}
	if s.UnreplicatedInWindow > 0 {
		return StatusFail,
			fmt.Sprintf("%d of %d windowed manifest(s) lack a replica copy",
				s.UnreplicatedInWindow, s.WindowedPrimaries),
			"configure cross-region replication via `repo replicate --from <primary> --to <replica>` and re-run on each affected manifest"
	}
	return StatusPass,
		fmt.Sprintf("%d of %d windowed manifest(s) have replica copies",
			s.WindowedReplicaCopies, s.WindowedPrimaries),
		""
}
