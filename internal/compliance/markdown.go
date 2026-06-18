// markdown.go — forensics-grade Markdown renderer for compliance reports (auditor-friendly layout).
package compliance

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// RenderMarkdown writes the report as a forensics-grade Markdown
// document. Layout is deliberately auditor-friendly:
//
//   - Top-of-page metadata (window, repo, mode, generated-at).
//   - One H2 per section in a fixed order so a hand-comparison
//     across two reports lines up paragraph-by-paragraph.
//   - Compliance-control hints in italics next to each section
//     (e.g. "_SOC 2 CC6.7, ISO 27001 A.8.13_") — these are
//     guidance, not assertions; an auditor still maps controls
//     to evidence themselves.
//   - Tables for everything tabular.
//   - Sentences for headlines (encryption coverage, verification
//     coverage) so a skim catches the % numbers without diving
//     into the table.
//
// The Markdown is GitHub-flavoured (uses GFM tables); renders
// cleanly in `gh issue body`, `git diff`, every Markdown viewer
// most operators have.
func RenderMarkdown(w io.Writer, r *Report) error {
	if r == nil {
		return fmt.Errorf("compliance: nil Report")
	}
	bw := &strings.Builder{}
	writeHeader(bw, r)
	writeBackupSection(bw, r.Backups)
	writeEncryptionSection(bw, r.Encryption)
	writeVerificationSection(bw, r.Verification)
	writeKEKLifecycleSection(bw, r.KEKLifecycle)
	writeApprovalSection(bw, r.Approvals)
	writeHoldSection(bw, r.Holds)
	writeReplicaSection(bw, r.Replicas)
	writeChainSection(bw, r.Chain)
	writeWORMSection(bw, r.WORM)
	writeFooter(bw, r)
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n")+"\n")
	return err
}

func writeHeader(bw *strings.Builder, r *Report) {
	fmt.Fprintln(bw, "# pg_hardstorage compliance report")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "| Field | Value |")
	fmt.Fprintln(bw, "| --- | --- |")
	fmt.Fprintf(bw, "| Repository | `%s` |\n", r.URL)
	if r.Repo != nil {
		if r.Repo.ID != "" {
			fmt.Fprintf(bw, "| Repository ID | `%s` |\n", r.Repo.ID)
		}
		if r.Repo.Mode != "" {
			fmt.Fprintf(bw, "| Mode | %s |\n", r.Repo.Mode)
		}
	}
	fmt.Fprintf(bw, "| Window start | %s |\n", r.Since.Format(time.RFC3339))
	fmt.Fprintf(bw, "| Window end | %s |\n", r.Until.Format(time.RFC3339))
	if r.DeploymentFilter != "" {
		fmt.Fprintf(bw, "| Filter | deployment `%s` |\n", r.DeploymentFilter)
	}
	fmt.Fprintf(bw, "| Generated at | %s |\n", r.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "| Walk duration | %d ms |\n", r.DurationMS)
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "_This report is a record of FACTS over the named window. It is not a verdict — auditors map controls to evidence themselves._")
	fmt.Fprintln(bw)
}

func writeBackupSection(bw *strings.Builder, b *BackupSection) {
	fmt.Fprintln(bw, "## Backup activity")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "_SOC 2 CC7.3 (system monitoring), ISO 27001 A.12.3 (information backup)._")
	fmt.Fprintln(bw)
	if b == nil {
		fmt.Fprintln(bw, "(skipped)")
		fmt.Fprintln(bw)
		return
	}
	if b.TotalCommitted == 0 {
		fmt.Fprintln(bw, "No backups committed in this window.")
		fmt.Fprintln(bw)
		return
	}
	fmt.Fprintf(bw, "**%d backup(s) committed** in this window.\n\n", b.TotalCommitted)
	if !b.OldestStoppedAt.IsZero() {
		fmt.Fprintf(bw, "Earliest committed: %s. Latest committed: %s.\n\n",
			b.OldestStoppedAt.Format(time.RFC3339),
			b.NewestStoppedAt.Format(time.RFC3339))
	}
	if len(b.ByType) > 0 {
		fmt.Fprintln(bw, "**By type:**")
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "| Type | Count |")
		fmt.Fprintln(bw, "| --- | ---: |")
		types := sortedMapKeys(b.ByType)
		for _, k := range types {
			fmt.Fprintf(bw, "| %s | %d |\n", k, b.ByType[k])
		}
		fmt.Fprintln(bw)
	}
	if len(b.ByDeployment) > 0 {
		fmt.Fprintln(bw, "**By deployment:**")
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "| Deployment | Total | Full | Incremental | Earliest | Latest |")
		fmt.Fprintln(bw, "| --- | ---: | ---: | ---: | --- | --- |")
		for _, d := range b.ByDeployment {
			fmt.Fprintf(bw, "| `%s` | %d | %d | %d | %s | %s |\n",
				d.Deployment, d.BackupCount, d.FullCount, d.IncCount,
				formatTime(d.OldestStopAt), formatTime(d.NewestStopAt))
		}
		fmt.Fprintln(bw)
	}
}

func writeEncryptionSection(bw *strings.Builder, e *EncryptionSection) {
	fmt.Fprintln(bw, "## Encryption coverage")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "_SOC 2 CC6.7 (data-at-rest encryption), ISO 27001 A.8.24 (cryptography), HIPAA §164.312(a)(2)(iv)._")
	fmt.Fprintln(bw)
	if e == nil {
		fmt.Fprintln(bw, "(skipped)")
		fmt.Fprintln(bw)
		return
	}
	total := e.EncryptedCount + e.UnencryptedCount
	if total == 0 {
		fmt.Fprintln(bw, "No backups in window — coverage is not applicable.")
		fmt.Fprintln(bw)
		return
	}
	icon := "✓"
	if e.UnencryptedCount > 0 {
		icon = "·"
	}
	fmt.Fprintf(bw, "**%s Encrypted: %s** (%d of %d backups)\n\n",
		icon, FormatPercent(e.CoveragePercent), e.EncryptedCount, total)
	if len(e.SchemesUsed) > 0 {
		fmt.Fprintf(bw, "Schemes in use: `%s`.\n\n", strings.Join(e.SchemesUsed, "`, `"))
	}
	if len(e.ByKEKRef) > 0 {
		fmt.Fprintln(bw, "**By KEK ref:**")
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "| KEK ref | Manifests |")
		fmt.Fprintln(bw, "| --- | ---: |")
		for _, k := range e.ByKEKRef {
			fmt.Fprintf(bw, "| `%s` | %d |\n", k.KEKRef, k.ManifestCount)
		}
		fmt.Fprintln(bw)
	}
}

func writeVerificationSection(bw *strings.Builder, v *VerificationSection) {
	fmt.Fprintln(bw, "## Verification coverage")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "_SOC 2 CC7.5 (recovery testing), ISO 27001 A.5.30 (ICT readiness for business continuity)._")
	fmt.Fprintln(bw)
	if v == nil {
		fmt.Fprintln(bw, "(skipped)")
		fmt.Fprintln(bw)
		return
	}
	if v.TotalRuns == 0 {
		fmt.Fprintln(bw, "No verification runs recorded in this window.")
		fmt.Fprintln(bw)
		return
	}
	fmt.Fprintf(bw, "**%d verification run(s)** in this window.\n\n", v.TotalRuns)
	if len(v.ByOutcome) > 0 {
		fmt.Fprintln(bw, "**By outcome:**")
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "| Outcome | Runs |")
		fmt.Fprintln(bw, "| --- | ---: |")
		for _, k := range sortedMapKeys(v.ByOutcome) {
			fmt.Fprintf(bw, "| %s | %d |\n", k, v.ByOutcome[k])
		}
		fmt.Fprintln(bw)
	}
	if len(v.ByDeployment) > 0 {
		fmt.Fprintln(bw, "**By deployment:**")
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "| Deployment | Runs | Earliest | Last |")
		fmt.Fprintln(bw, "| --- | ---: | --- | --- |")
		for _, d := range v.ByDeployment {
			fmt.Fprintf(bw, "| `%s` | %d | %s | %s |\n",
				d.Deployment, d.Runs, formatTime(d.OldestRunAt), formatTime(d.LastRunAt))
		}
		fmt.Fprintln(bw)
	}
}

func writeKEKLifecycleSection(bw *strings.Builder, k *KEKLifecycleSection) {
	fmt.Fprintln(bw, "## KEK lifecycle")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "_SOC 2 CC6.1 (logical access — key custody), ISO 27001 A.8.24, PCI DSS 3.6 (key management)._")
	fmt.Fprintln(bw)
	if k == nil {
		fmt.Fprintln(bw, "(skipped)")
		fmt.Fprintln(bw)
		return
	}
	if k.RotationsAttempted == 0 && k.ShredsAttempted == 0 {
		fmt.Fprintln(bw, "No KEK lifecycle activity recorded in this window.")
		fmt.Fprintln(bw)
		return
	}
	fmt.Fprintf(bw, "**Rotations attempted:** %d (succeeded: %d). **Shred attempts:** %d.\n\n",
		k.RotationsAttempted, k.RotationsSucceeded, k.ShredsAttempted)
	if len(k.Events) > 0 {
		fmt.Fprintln(bw, "**Events:**")
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "| When | Action | Actor | Tenant | Old ref | New ref |")
		fmt.Fprintln(bw, "| --- | --- | --- | --- | --- | --- |")
		for _, e := range k.Events {
			fmt.Fprintf(bw, "| %s | `%s` | %s | %s | %s | %s |\n",
				formatTime(e.Timestamp), e.Action,
				orPlaceholder(e.Actor), orPlaceholder(e.Tenant),
				codeOrPlaceholder(e.OldRef), codeOrPlaceholder(e.NewRef))
		}
		fmt.Fprintln(bw)
	}
}

func writeApprovalSection(bw *strings.Builder, a *ApprovalSection) {
	fmt.Fprintln(bw, "## Approval workflow")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "_SOC 2 CC6.6 (logical access — separation of duties), ISO 27001 A.5.18 (access rights)._")
	fmt.Fprintln(bw)
	if a == nil {
		fmt.Fprintln(bw, "(skipped)")
		fmt.Fprintln(bw)
		return
	}
	fmt.Fprintf(bw, "**Approval requests created:** %d. **Destructive ops executed:** %d.\n\n",
		a.RequestsCreated, a.DestructiveOps)
	if len(a.ByStatus) > 0 {
		fmt.Fprintln(bw, "**Approval-event counts (by event type):**")
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "| Event | Count |")
		fmt.Fprintln(bw, "| --- | ---: |")
		for _, k := range sortedMapKeys(a.ByStatus) {
			fmt.Fprintf(bw, "| %s | %d |\n", k, a.ByStatus[k])
		}
		fmt.Fprintln(bw)
	}
	if len(a.DestructiveByOp) > 0 {
		fmt.Fprintln(bw, "**Destructive ops by action:**")
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "| Action | Count |")
		fmt.Fprintln(bw, "| --- | ---: |")
		for _, k := range sortedMapKeys(a.DestructiveByOp) {
			fmt.Fprintf(bw, "| `%s` | %d |\n", k, a.DestructiveByOp[k])
		}
		fmt.Fprintln(bw)
	}
}

func writeHoldSection(bw *strings.Builder, h *HoldSection) {
	fmt.Fprintln(bw, "## Holds")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "_eDiscovery / legal-hold trace; ISO 27001 A.5.34 (privacy and protection of PII)._")
	fmt.Fprintln(bw)
	if h == nil {
		fmt.Fprintln(bw, "(skipped)")
		fmt.Fprintln(bw)
		return
	}
	if h.HoldsAdded == 0 && h.HoldsRemoved == 0 && h.HoldsExpired == 0 {
		fmt.Fprintln(bw, "No hold-marker activity recorded in this window.")
		fmt.Fprintln(bw)
		return
	}
	fmt.Fprintln(bw, "| Lifecycle event | Count |")
	fmt.Fprintln(bw, "| --- | ---: |")
	if h.HoldsAdded > 0 {
		fmt.Fprintf(bw, "| `hold.add` | %d |\n", h.HoldsAdded)
	}
	if h.HoldsRemoved > 0 {
		fmt.Fprintf(bw, "| `hold.remove` | %d |\n", h.HoldsRemoved)
	}
	if h.HoldsExpired > 0 {
		fmt.Fprintf(bw, "| `hold.expire` | %d |\n", h.HoldsExpired)
	}
	fmt.Fprintln(bw)
}

func writeReplicaSection(bw *strings.Builder, r *ReplicaSection) {
	fmt.Fprintln(bw, "## Replica completeness")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "_SOC 2 A1.2 (capacity / availability), ISO 27001 A.8.13 (information backup)._")
	fmt.Fprintln(bw)
	if r == nil {
		fmt.Fprintln(bw, "(skipped)")
		fmt.Fprintln(bw)
		return
	}
	if r.WindowedPrimaries == 0 {
		fmt.Fprintln(bw, "No primary manifests committed in this window.")
		fmt.Fprintln(bw)
		return
	}
	pct := float64(r.WindowedReplicaCopies) * 100.0 / float64(r.WindowedPrimaries)
	icon := "✓"
	if r.UnreplicatedInWindow > 0 {
		icon = "✗"
	}
	fmt.Fprintf(bw, "**%s Replica coverage: %s** (%d of %d windowed primaries have a replica copy; %d unreplicated).\n\n",
		icon, FormatPercent(pct), r.WindowedReplicaCopies, r.WindowedPrimaries, r.UnreplicatedInWindow)
}

func writeChainSection(bw *strings.Builder, c *ChainSection) {
	fmt.Fprintln(bw, "## Audit chain")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "_SOC 2 CC7.2 (anomaly detection), ISO 27001 A.8.15 (logging), HIPAA §164.312(b) (audit controls)._")
	fmt.Fprintln(bw)
	if c == nil {
		fmt.Fprintln(bw, "(skipped)")
		fmt.Fprintln(bw)
		return
	}
	icon := "✓"
	verifyDescr := "PASS"
	if !c.VerifyOK {
		icon = "✗"
		verifyDescr = fmt.Sprintf("FAIL (%d hash mismatches, %d chain breaks)",
			c.VerifyHashMismatches, c.VerifyChainBreaks)
	}
	fmt.Fprintf(bw, "**%s Chain verify: %s** (%d events checked).\n\n",
		icon, verifyDescr, c.VerifyEventsChecked)
	fmt.Fprintf(bw, "Events in window: %d. Anchors in window: %d.\n\n",
		c.EventsInWindow, c.AnchorsInWindow)
	if !c.LastAnchorAt.IsZero() {
		ageDescr := "(unknown)"
		if c.LastAnchorAgeMS > 0 {
			ageDescr = (time.Duration(c.LastAnchorAgeMS) * time.Millisecond).Truncate(time.Second).String() + " ago"
		}
		fmt.Fprintf(bw, "Last anchor at %s %s.\n\n",
			c.LastAnchorAt.Format(time.RFC3339), ageDescr)
	}
}

func writeWORMSection(bw *strings.Builder, w *WORMSection) {
	fmt.Fprintln(bw, "## WORM (write-once-read-many) status")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "_SOC 2 CC6.7 (encryption + retention), ISO 27001 A.8.13 (information backup), SEC Rule 17a-4 (broker-dealer record retention)._")
	fmt.Fprintln(bw)
	if w == nil {
		fmt.Fprintln(bw, "(skipped)")
		fmt.Fprintln(bw)
		return
	}
	if !w.Active {
		fmt.Fprintln(bw, "WORM is **not configured** for this repository.")
		fmt.Fprintln(bw)
		return
	}
	fmt.Fprintf(bw, "WORM is **active** in `%s` mode with retention `%s` (%d seconds).\n\n",
		w.Mode, w.Retention, w.RetentionSeconds)
}

func writeFooter(bw *strings.Builder, r *Report) {
	fmt.Fprintln(bw, "---")
	fmt.Fprintln(bw)
	fmt.Fprintf(bw, "_Schema `%s`. Report walked %s in %d ms._\n",
		r.Schema, r.URL, r.DurationMS)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format(time.RFC3339)
}

func orPlaceholder(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func codeOrPlaceholder(s string) string {
	if s == "" {
		return "—"
	}
	return "`" + s + "`"
}

// sortedMapKeys returns the keys of an int-valued map in
// alphabetical order. Stable iteration → stable Markdown output.
func sortedMapKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
