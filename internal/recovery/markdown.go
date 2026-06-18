// markdown.go — forensics-grade Markdown renderer for the recovery readiness report.
package recovery

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// RenderReadinessMarkdown writes the ReadinessReport as a forensics-
// grade GFM document. Same posture as the compliance + forecast
// renderers: top-of-page metadata, fixed-order H2 sections, ✓/✗
// glyphs on headlines.
func RenderReadinessMarkdown(w io.Writer, r *ReadinessReport) error {
	if r == nil {
		return fmt.Errorf("recovery: nil ReadinessReport")
	}
	bw := &strings.Builder{}
	writeReadinessHeader(bw, r)
	writeReadinessVerdict(bw, r)
	writeReadinessLatest(bw, r)
	writeReadinessRPO(bw, r)
	writeReadinessRTO(bw, r)
	writeReadinessVerification(bw, r)
	writeReadinessEncryption(bw, r)
	writeReadinessWAL(bw, r)
	writeReadinessIssues(bw, r)
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n")+"\n")
	return err
}

func writeReadinessHeader(bw *strings.Builder, r *ReadinessReport) {
	fmt.Fprintf(bw, "# pg_hardstorage recovery readiness — `%s`\n\n", r.Deployment)
	fmt.Fprintln(bw, "| Field | Value |")
	fmt.Fprintln(bw, "| --- | --- |")
	fmt.Fprintf(bw, "| Repository | `%s` |\n", r.URL)
	fmt.Fprintf(bw, "| Deployment | `%s` |\n", r.Deployment)
	fmt.Fprintf(bw, "| Backups available | %d |\n", r.BackupCount)
	if !r.OldestStoppedAt.IsZero() {
		fmt.Fprintf(bw, "| Oldest backup | %s |\n", r.OldestStoppedAt.Format(time.RFC3339))
	}
	fmt.Fprintf(bw, "| Generated at | %s |\n", r.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "| Walk duration | %d ms |\n", r.DurationMS)
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "_This report aggregates the signals an operator needs to answer \"if I had to recover this deployment right now, would it work, and how long would it take?\". Read-only; safe at any cadence._")
	fmt.Fprintln(bw)
}

func writeReadinessVerdict(bw *strings.Builder, r *ReadinessReport) {
	fmt.Fprintln(bw, "## Verdict")
	fmt.Fprintln(bw)
	icon := "✓"
	descr := "ready"
	switch r.OverallStatus {
	case StatusReadyWithWarn:
		icon = "·"
		descr = "ready with warnings"
	case StatusNotReady:
		icon = "✗"
		descr = "not ready"
	case StatusNoBackups:
		icon = "✗"
		descr = "no backups committed for this deployment"
	}
	fmt.Fprintf(bw, "**%s %s** — %d issue(s) recorded.\n\n",
		icon, strings.ToUpper(descr), len(r.Issues))
}

func writeReadinessLatest(bw *strings.Builder, r *ReadinessReport) {
	fmt.Fprintln(bw, "## Latest backup")
	fmt.Fprintln(bw)
	if r.Latest == nil {
		fmt.Fprintln(bw, "_No backups committed for this deployment._")
		fmt.Fprintln(bw)
		return
	}
	l := r.Latest
	fmt.Fprintln(bw, "| Field | Value |")
	fmt.Fprintln(bw, "| --- | --- |")
	fmt.Fprintf(bw, "| Backup ID | `%s` |\n", l.BackupID)
	fmt.Fprintf(bw, "| Type | %s |\n", l.Type)
	fmt.Fprintf(bw, "| Stopped at | %s |\n", l.StoppedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "| Age | %s |\n", durationFromSeconds(l.AgeSeconds))
	fmt.Fprintf(bw, "| Stop LSN | `%s` |\n", l.StopLSN)
	fmt.Fprintf(bw, "| Timeline | %d |\n", l.Timeline)
	fmt.Fprintf(bw, "| PG version | %d |\n", l.PGVersion)
	fmt.Fprintf(bw, "| Logical bytes | %s |\n", humanBytes(l.LogicalBytes))
	fmt.Fprintf(bw, "| Encrypted | %v |\n", l.Encrypted)
	if l.KEKRef != "" {
		fmt.Fprintf(bw, "| KEK ref | `%s` |\n", l.KEKRef)
	}
	fmt.Fprintf(bw, "| Replica copy | %v |\n", l.HasReplicaCopy)
	if l.WALGapCount > 0 {
		fmt.Fprintf(bw, "| Manifest WAL gaps | %d (PITR within those ranges is refused) |\n",
			l.WALGapCount)
	}
	fmt.Fprintln(bw)
}

func writeReadinessRPO(bw *strings.Builder, r *ReadinessReport) {
	fmt.Fprintln(bw, "## RPO (recovery-point objective)")
	fmt.Fprintln(bw)
	if r.RPO == nil {
		fmt.Fprintln(bw, "(skipped)")
		fmt.Fprintln(bw)
		return
	}
	fmt.Fprintf(bw, "**Observed:** %s.\n\n", durationFromSeconds(r.RPO.ObservedSeconds))
	if r.RPO.TargetSeconds > 0 {
		icon := "✓"
		if !r.RPO.Met {
			icon = "✗"
		}
		fmt.Fprintf(bw, "**Target:** %s. **%s** target met.\n\n",
			durationFromSeconds(r.RPO.TargetSeconds), icon)
	} else {
		fmt.Fprintln(bw, "_No RPO target configured (`pg_hardstorage slo set`)._")
		fmt.Fprintln(bw)
	}
}

func writeReadinessRTO(bw *strings.Builder, r *ReadinessReport) {
	fmt.Fprintln(bw, "## RTO (recovery-time objective)")
	fmt.Fprintln(bw)
	if r.RTO == nil {
		fmt.Fprintln(bw, "(skipped)")
		fmt.Fprintln(bw)
		return
	}
	fmt.Fprintf(bw, "**Estimated:** %s at assumed throughput %s.\n\n",
		durationFromSeconds(r.RTO.EstimatedSeconds),
		humanThroughput(r.RTO.AssumedThroughputBytes))
	if r.RTO.TargetSeconds > 0 {
		icon := "✓"
		if !r.RTO.Met {
			icon = "✗"
		}
		fmt.Fprintf(bw, "**Target:** %s. **%s** target met.\n\n",
			durationFromSeconds(r.RTO.TargetSeconds), icon)
	}
	fmt.Fprintln(bw, "_Throughput is assumed; the real restore-time depends on the destination's NIC + disk + decryption / decompression overhead. Run `pg_hardstorage verify --full` to measure._")
	fmt.Fprintln(bw)
}

func writeReadinessVerification(bw *strings.Builder, r *ReadinessReport) {
	fmt.Fprintln(bw, "## Verification freshness")
	fmt.Fprintln(bw)
	if r.Verification == nil {
		fmt.Fprintln(bw, "(skipped)")
		fmt.Fprintln(bw)
		return
	}
	v := r.Verification
	if !v.HasRecord {
		fmt.Fprintln(bw, "_No `verification.json` next to the latest manifest._")
		fmt.Fprintln(bw)
		return
	}
	icon := "✓"
	descr := "fresh"
	if v.Stale {
		icon = "·"
		descr = "stale"
	}
	fmt.Fprintf(bw, "**%s Verification %s** — last run %s ago (threshold %s).\n\n",
		icon, descr,
		durationFromSeconds(v.AgeSeconds),
		durationFromSeconds(v.StalenessWindowSeconds))
}

func writeReadinessEncryption(bw *strings.Builder, r *ReadinessReport) {
	fmt.Fprintln(bw, "## Encryption health")
	fmt.Fprintln(bw)
	if r.Encryption == nil {
		fmt.Fprintln(bw, "(skipped)")
		fmt.Fprintln(bw)
		return
	}
	e := r.Encryption
	if !e.Encrypted {
		fmt.Fprintln(bw, "_Latest backup is plaintext. The `kms verify` command can audit the fleet's encryption coverage if a policy decision is needed._")
		fmt.Fprintln(bw)
		return
	}
	icon := "✓"
	descr := "reachable"
	if !e.KEKReachable {
		icon = "✗"
		descr = "NOT reachable"
	}
	fmt.Fprintf(bw, "**%s KEK `%s` is %s.**\n\n", icon, e.KEKRef, descr)
	if e.Note != "" {
		fmt.Fprintf(bw, "Note: %s\n\n", e.Note)
	}
}

func writeReadinessWAL(bw *strings.Builder, r *ReadinessReport) {
	fmt.Fprintln(bw, "## WAL coverage")
	fmt.Fprintln(bw)
	if r.WAL == nil {
		fmt.Fprintln(bw, "(skipped)")
		fmt.Fprintln(bw)
		return
	}
	w := r.WAL
	if !w.HasArchivedWAL {
		fmt.Fprintln(bw, "_No archived WAL — base-only recovery is possible; PITR past `stop_lsn` is not._")
		fmt.Fprintln(bw)
		return
	}
	fmt.Fprintf(bw, "**Highest archived LSN:** `%s`.\n\n", w.HighestArchivedLSN)
	if w.HasGapPersisted {
		fmt.Fprintf(bw, "**✗ Persisted WAL gap:** %d bytes (`%s..%s`) detected %s.\n\n",
			w.GapBytes, w.GapStartLSN, w.GapEndLSN,
			w.GapDetectedAt.Format(time.RFC3339))
	} else {
		fmt.Fprintln(bw, "✓ No persisted WAL gaps recorded.")
		fmt.Fprintln(bw)
	}
}

func writeReadinessIssues(bw *strings.Builder, r *ReadinessReport) {
	fmt.Fprintln(bw, "## Issues")
	fmt.Fprintln(bw)
	if len(r.Issues) == 0 {
		fmt.Fprintln(bw, "_No issues — the deployment is recovery-ready._")
		fmt.Fprintln(bw)
		return
	}
	issues := append([]ReadinessIssue(nil), r.Issues...)
	SortIssues(issues)
	fmt.Fprintln(bw, "| Severity | Code | Message | Suggestion |")
	fmt.Fprintln(bw, "| --- | --- | --- | --- |")
	for _, i := range issues {
		fmt.Fprintf(bw, "| %s | `%s` | %s | %s |\n",
			i.Severity, i.Code, i.Message, fallbackS(i.Suggestion, "—"))
	}
	fmt.Fprintln(bw)
}

// RenderWindowsMarkdown writes a WindowsReport as a forensics-grade
// GFM document.
func RenderWindowsMarkdown(w io.Writer, r *WindowsReport) error {
	if r == nil {
		return fmt.Errorf("recovery: nil WindowsReport")
	}
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "# pg_hardstorage recovery windows — `%s`\n\n", r.Deployment)
	fmt.Fprintln(bw, "| Field | Value |")
	fmt.Fprintln(bw, "| --- | --- |")
	fmt.Fprintf(bw, "| Repository | `%s` |\n", r.URL)
	fmt.Fprintf(bw, "| Deployment | `%s` |\n", r.Deployment)
	fmt.Fprintf(bw, "| Windows | %d |\n", r.Coverage.WindowCount)
	if !r.Coverage.EarliestRecoverableTime.IsZero() {
		fmt.Fprintf(bw, "| Earliest recoverable | %s |\n",
			r.Coverage.EarliestRecoverableTime.Format(time.RFC3339))
	}
	if !r.Coverage.LatestRecoverableTime.IsZero() {
		fmt.Fprintf(bw, "| Latest recoverable | %s |\n",
			r.Coverage.LatestRecoverableTime.Format(time.RFC3339))
	}
	if r.Coverage.WindowsWithGaps > 0 {
		fmt.Fprintf(bw, "| ✗ Windows with WAL gaps | %d |\n", r.Coverage.WindowsWithGaps)
	}
	if r.Coverage.TotalGapBytes > 0 {
		fmt.Fprintf(bw, "| ✗ Total gap bytes | %d |\n", r.Coverage.TotalGapBytes)
	}
	fmt.Fprintf(bw, "| Generated at | %s |\n", r.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "| Walk duration | %d ms |\n", r.DurationMS)
	fmt.Fprintln(bw)
	if r.Coverage.WindowCount == 0 {
		fmt.Fprintln(bw, "_No PITR windows — the deployment has no committed backups._")
		fmt.Fprintln(bw)
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n")+"\n")
		return err
	}
	fmt.Fprintln(bw, "## PITR windows (newest first)")
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "| # | Backup | Stopped at | TLI | Earliest LSN | Latest LSN | WAL? | Replica? | Gaps |")
	fmt.Fprintln(bw, "| --- | --- | --- | --- | --- | --- | --- | --- | --- |")
	for i, win := range r.Windows {
		walIcon := "✗"
		if win.HasArchivedWAL {
			walIcon = "✓"
		}
		repIcon := "✗"
		if win.HasReplicaCopy {
			repIcon = "✓"
		}
		gaps := len(win.Gaps) + len(win.WALGapsFromManifest)
		gapTxt := "0"
		if gaps > 0 {
			gapTxt = fmt.Sprintf("✗ %d", gaps)
		}
		fmt.Fprintf(bw, "| %d | `%s` | %s | %d | `%s` | `%s` | %s | %s | %s |\n",
			i+1,
			win.BackupID,
			win.StoppedAt.Format(time.RFC3339),
			win.Timeline,
			win.EarliestRestoreLSN,
			fallbackS(win.LatestRestoreLSN, "—"),
			walIcon, repIcon, gapTxt)
	}
	fmt.Fprintln(bw)

	// Per-window gap detail when any window has gaps.
	if r.Coverage.WindowsWithGaps > 0 {
		fmt.Fprintln(bw, "## Gap detail")
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "| Backup | Source | Range | Bytes | Slot | Detected at |")
		fmt.Fprintln(bw, "| --- | --- | --- | --- | --- | --- |")
		for _, win := range r.Windows {
			for _, g := range win.WALGapsFromManifest {
				fmt.Fprintf(bw, "| `%s` | %s | `%s..%s` | %d | `%s` | %s |\n",
					win.BackupID, g.Source, g.StartLSN, g.EndLSN, g.Bytes,
					fallbackS(g.SlotName, "—"), g.DetectedAt.Format(time.RFC3339))
			}
			for _, g := range win.Gaps {
				fmt.Fprintf(bw, "| `%s` | %s | `%s..%s` | %d | `%s` | %s |\n",
					win.BackupID, g.Source, g.StartLSN, g.EndLSN, g.Bytes,
					fallbackS(g.SlotName, "—"), g.DetectedAt.Format(time.RFC3339))
			}
		}
		fmt.Fprintln(bw)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n")+"\n")
	return err
}

// RenderDrillMarkdown writes a DrillReport as a forensics-grade
// GFM document.  Sections in fixed order: Header → Verdict →
// Phases → RTO → Restore detail → Verify detail → Issues.
func RenderDrillMarkdown(w io.Writer, r *DrillReport) error {
	if r == nil {
		return fmt.Errorf("recovery: nil DrillReport")
	}
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "# pg_hardstorage recovery drill — `%s/%s`\n\n",
		r.Deployment, r.BackupID)
	fmt.Fprintln(bw, "| Field | Value |")
	fmt.Fprintln(bw, "| --- | --- |")
	fmt.Fprintf(bw, "| Repository | `%s` |\n", r.URL)
	fmt.Fprintf(bw, "| Deployment | `%s` |\n", r.Deployment)
	if r.BackupID != "" {
		fmt.Fprintf(bw, "| Backup ID | `%s` |\n", r.BackupID)
	}
	if r.TargetDir != "" {
		fmt.Fprintf(bw, "| Target dir | `%s` |\n", r.TargetDir)
	}
	fmt.Fprintf(bw, "| Generated at | %s |\n", r.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "| Walk duration | %d ms |\n", r.DurationMS)
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "_The drill takes a real backup, restores into a temporary directory, runs `pg_verifybackup` against the restored data dir, and tears down.  RTO actual is the wallclock time from drill start to successful restore (excluding verify).  Read-only against the source repo._")
	fmt.Fprintln(bw)

	// Verdict section.
	fmt.Fprintln(bw, "## Verdict")
	fmt.Fprintln(bw)
	icon, descr := drillVerdictGlyph(r.Verdict)
	fmt.Fprintf(bw, "**%s %s** — %d issue(s) recorded.\n\n",
		icon, strings.ToUpper(descr), len(r.Issues))

	// Phase table.
	fmt.Fprintln(bw, "## Phases")
	fmt.Fprintln(bw)
	if len(r.Phases) == 0 {
		fmt.Fprintln(bw, "_(no phases recorded)_")
		fmt.Fprintln(bw)
	} else {
		fmt.Fprintln(bw, "| # | Phase | OK | Duration | Note |")
		fmt.Fprintln(bw, "| --- | --- | --- | --- | --- |")
		for i, p := range r.Phases {
			ok := "✓"
			if !p.OK {
				ok = "✗"
			}
			note := p.Note
			if p.Error != "" {
				note = "ERROR: " + p.Error
			}
			fmt.Fprintf(bw, "| %d | `%s` | %s | %d ms | %s |\n",
				i+1, p.Name, ok, p.DurationMS, fallbackS(note, "—"))
		}
		fmt.Fprintln(bw)
	}

	// RTO section.
	fmt.Fprintln(bw, "## RTO actual vs target")
	fmt.Fprintln(bw)
	if r.RTOActualSeconds > 0 {
		fmt.Fprintf(bw, "**Actual RTO:** %s.\n\n",
			durationFromSeconds(r.RTOActualSeconds))
	} else {
		fmt.Fprintln(bw, "_RTO actual not recorded (restore phase did not complete)._")
		fmt.Fprintln(bw)
	}
	if r.RTOEstimateSeconds > 0 {
		ic := "✓"
		descr := "within budget"
		if r.RTOActualSeconds > r.RTOEstimateSeconds {
			ic = "✗"
			descr = "exceeds budget"
		}
		fmt.Fprintf(bw, "**Estimate / target:** %s.  **%s** %s.\n\n",
			durationFromSeconds(r.RTOEstimateSeconds), ic, descr)
	} else {
		fmt.Fprintln(bw, "_No RTO target supplied; pass --rto-seconds to the CLI for actual-vs-target comparison._")
		fmt.Fprintln(bw)
	}

	// Restore detail.
	fmt.Fprintln(bw, "## Restore detail")
	fmt.Fprintln(bw)
	if r.Restore == nil {
		fmt.Fprintln(bw, "_(restore phase did not complete)_")
		fmt.Fprintln(bw)
	} else {
		fmt.Fprintln(bw, "| Field | Value |")
		fmt.Fprintln(bw, "| --- | --- |")
		fmt.Fprintf(bw, "| Files materialised | %d |\n", r.Restore.FileCount)
		fmt.Fprintf(bw, "| Bytes written | %s |\n", humanBytes(r.Restore.BytesWritten))
		fmt.Fprintf(bw, "| Chunks fetched | %d |\n", r.Restore.ChunksFetched)
		fmt.Fprintf(bw, "| Backup-label size | %d B |\n", r.Restore.BackupLabelSize)
		fmt.Fprintf(bw, "| Tablespace-map size | %d B |\n", r.Restore.TablespaceMapSize)
		fmt.Fprintf(bw, "| Started at | %s |\n", r.Restore.StartedAt.Format(time.RFC3339))
		fmt.Fprintf(bw, "| Stopped at | %s |\n", r.Restore.StoppedAt.Format(time.RFC3339))
		fmt.Fprintln(bw)
	}

	// Verify detail.
	fmt.Fprintln(bw, "## Verify detail")
	fmt.Fprintln(bw)
	if r.Verify == nil {
		fmt.Fprintln(bw, "_(verify phase did not run)_")
		fmt.Fprintln(bw)
	} else {
		fmt.Fprintln(bw, "| Field | Value |")
		fmt.Fprintln(bw, "| --- | --- |")
		fmt.Fprintf(bw, "| Tool | `%s` |\n", r.Verify.Tool)
		fmt.Fprintf(bw, "| Image | `%s` |\n", r.Verify.Image)
		fmt.Fprintf(bw, "| PG major | %s |\n", r.Verify.PGMajor)
		passDescr := "✓ passed"
		if r.Verify.Skipped {
			passDescr = "· skipped (" + r.Verify.SkipReason + ")"
		} else if !r.Verify.Passed {
			passDescr = "✗ failed"
		}
		fmt.Fprintf(bw, "| Outcome | %s |\n", passDescr)
		fmt.Fprintf(bw, "| Started at | %s |\n", r.Verify.StartedAt.Format(time.RFC3339))
		fmt.Fprintf(bw, "| Stopped at | %s |\n", r.Verify.StoppedAt.Format(time.RFC3339))
		fmt.Fprintln(bw)
		if r.Verify.Stderr != "" {
			fmt.Fprintln(bw, "**Verify stderr:**")
			fmt.Fprintln(bw)
			fmt.Fprintln(bw, "```")
			fmt.Fprintln(bw, strings.TrimRight(r.Verify.Stderr, "\n"))
			fmt.Fprintln(bw, "```")
			fmt.Fprintln(bw)
		}
	}

	// Issues.
	fmt.Fprintln(bw, "## Issues")
	fmt.Fprintln(bw)
	if len(r.Issues) == 0 {
		fmt.Fprintln(bw, "_No issues — the drill ran clean._")
		fmt.Fprintln(bw)
	} else {
		issues := append([]ReadinessIssue(nil), r.Issues...)
		SortIssues(issues)
		fmt.Fprintln(bw, "| Severity | Code | Message | Suggestion |")
		fmt.Fprintln(bw, "| --- | --- | --- | --- |")
		for _, i := range issues {
			fmt.Fprintf(bw, "| %s | `%s` | %s | %s |\n",
				i.Severity, i.Code, i.Message, fallbackS(i.Suggestion, "—"))
		}
		fmt.Fprintln(bw)
	}

	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n")+"\n")
	return err
}

// drillVerdictGlyph returns the icon + descriptor for the verdict.
func drillVerdictGlyph(v DrillVerdict) (string, string) {
	switch v {
	case DrillVerdictPass:
		return "✓", "pass"
	case DrillVerdictPartial:
		return "·", "partial"
	default:
		return "✗", "fail"
	}
}

// durationFromSeconds renders an integer-second duration as a
// compact human form ("47s", "5m", "3h", "2d").
func durationFromSeconds(secs int64) string {
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	if secs < 60*60 {
		return fmt.Sprintf("%dm", secs/60)
	}
	if secs < 24*60*60 {
		return fmt.Sprintf("%dh", secs/3600)
	}
	return fmt.Sprintf("%dd", secs/(24*3600))
}

// humanBytes mirrors the implementation in forecast / repoaudit.
// Duplicated to avoid cross-package imports for a presentation
// helper.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	suffix := "KMGTPE"[exp]
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), suffix)
}

func fallbackS(s, dflt string) string {
	if s == "" {
		return dflt
	}
	return s
}
