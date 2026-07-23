// doctor.go — 'doctor' CLI verb: environment + path + PG/KMS/repo health diagnostics.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/recovery"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

// newRealDoctorCmd builds the real doctor command and is wired into
// NewRoot in place of a stub. For now it only exercises path resolution
// and config-load reporting; the PG / KMS / repo / slot health checks
// land in later slices.
//
// --exit-on-issues makes the documented exit-code-10 contract real
// (docs/reference/exit-codes.md): when issues at warning+ severity
// are present in the report, the command returns a structured
// `doctor.issues_present` error so the dispatcher routes to
// ExitDoctorIssues.  Without the flag the report still shows the
// issues but the command exits 0 — operators who script `doctor`
// for a healthcheck use the flag; humans browsing the report don't.
func newRealDoctorCmd() *cobra.Command {
	c := &cobra.Command{
		Use:          "doctor [<deployment>]",
		Short:        "Run health checks and suggest fixes",
		Long:         "Reports resolved filesystem paths, configuration status, and health checks.",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE:         runDoctor,
	}
	c.Flags().Bool("exit-on-issues", false,
		"exit with code 10 (ExitDoctorIssues) when the report contains issues at warning+ severity; "+
			"useful for cron / k8s liveness probes")
	c.Flags().Duration("drill-max-age", defaultDrillMaxAge,
		"maximum age of the last SUCCESSFUL recovery drill before doctor escalates recovery.drill_stale (CRITICAL)")
	return c
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	d := DispatcherFrom(cmd)

	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("doctor.path_resolve", err.Error()).Wrap(err)
	}

	cfg, cfgErr := config.Load(p)
	// We deliberately do NOT bubble cfgErr up — a bad config IS a finding,
	// not an early-return. doctor's job is to report; the report includes
	// the parse error instead of suppressing it.

	report := buildDoctorReport(p, cfg, cfgErr)

	// Defence-in-depth posture check: the top-level CLI gate (see
	// refuse_root.go) blocks euid 0 before any command body runs,
	// but doctor's job is to make hidden state legible.  If
	// someone bypassed the gate (e.g. via a legacy escape hatch
	// or a programmatic embedding), surface it loudly here.  In
	// the normal case this never fires because refuseRoot would
	// have aborted long before doctor's RunE.
	if geteuid() == 0 {
		report.Issues = append(report.Issues, doctorIssue{
			Severity: output.SeverityCritical,
			Code:     "agent.running_as_root",
			Message:  "pg_hardstorage is running as uid 0; the agent is designed to run as a dedicated system user (pgbackup on Debian/RPM; runAsNonRoot in k8s).  See refuse_root.go and the systemd unit at /lib/systemd/system/pg_hardstorage.service.",
		})
	}

	//: walk unique repo URLs from the deployment config and
	// run read-only health probes (chain length + anchor
	// freshness). A failed probe surfaces as an issue but doesn't
	// fail the doctor command — the rest of the report is still
	// useful.
	if cfg != nil && cfg.IsConfigured() {
		report.Repos, report.Issues = appendRepoChecks(cmd.Context(), cfg, report.Issues)
		report.WALGaps, report.Issues = appendWALGapChecks(cmd.Context(), cfg, report.Issues)
		report.ExpiredHolds, report.Issues = appendExpiredHoldChecks(cmd.Context(), cfg, report.Issues)
		report.PGVersions, report.Issues = appendPGVersionChecks(cmd.Context(), cfg, report.Issues)
		report.ManifestSig, report.Issues = appendManifestSignatureChecks(cmd.Context(), cfg, report.Issues)
		drillMaxAge, _ := cmd.Flags().GetDuration("drill-max-age")
		report.Drills, report.Issues = appendDrillChecks(cmd.Context(), cfg, drillMaxAge, report.Issues)
	}

	// Recompute Healthy from the FINAL issue set. buildDoctorReport
	// froze Healthy=true (flipping it false only for the config/path
	// checks it runs inline), but the root-euid check and the per-repo
	// probes above (appendRepoChecks/WALGaps/ManifestSig/…) append
	// error- and critical-severity issues AFTER that. Without this, a
	// JSON report could claim healthy:true alongside a critical issue.
	if doctorHasErrorOrHigher(report.Issues) {
		report.Healthy = false
	}

	res := output.NewResult(cmd.CommandPath()).WithBody(report)
	if err := d.Result(res); err != nil {
		return err
	}
	// --exit-on-issues: route ExitDoctorIssues (10) when the report
	// contains any issue at warning+ severity.  Documented in
	// docs/reference/exit-codes.md; cron / k8s liveness scripts
	// rely on this to alert without parsing the JSON body.
	exitOnIssues, _ := cmd.Flags().GetBool("exit-on-issues")
	if exitOnIssues && doctorHasWarningOrHigher(report.Issues) {
		// `doctor.issues_present` routes to ExitDoctorIssues via
		// the doctor.* namespace prefix mapping (see
		// internal/output/exitcode.go).
		return output.NewError("doctor.issues_present",
			fmt.Sprintf("doctor: %d issue(s) at warning+ severity (run without --exit-on-issues to see the report)",
				countDoctorIssues(report.Issues)))
	}
	return nil
}

// doctorHasWarningOrHigher reports whether any of the doctor's
// collected issues are at warning severity or above.  Anything
// below warning (info / notice) is informational; doctor
// surfaces them but doesn't escalate to a non-zero exit.
func doctorHasWarningOrHigher(issues []doctorIssue) bool {
	for _, i := range issues {
		if doctorSeverityRank(i.Severity) >= doctorSeverityRank(output.SeverityWarning) {
			return true
		}
	}
	return false
}

// doctorHasErrorOrHigher reports whether any collected issue is at
// error severity or above (error / critical / alert / emergency).
// This is the threshold at which the report's Healthy verdict flips
// to false — matching buildDoctorReport's inline SeverityError flips.
func doctorHasErrorOrHigher(issues []doctorIssue) bool {
	for _, i := range issues {
		if doctorSeverityRank(i.Severity) >= doctorSeverityRank(output.SeverityError) {
			return true
		}
	}
	return false
}

// countDoctorIssues returns how many of the collected issues are
// at warning+ severity (so the error message reports the
// actionable count, not the noisy total).
func countDoctorIssues(issues []doctorIssue) int {
	n := 0
	for _, i := range issues {
		if doctorSeverityRank(i.Severity) >= doctorSeverityRank(output.SeverityWarning) {
			n++
		}
	}
	return n
}

// doctorSeverityRank maps an output.Severity to a numeric rank
// for "warning+" comparisons.  output.Severity follows RFC 5424
// (lower number = more severe — SeverityEmergency=0,
// SeverityDebug=7), which is fine for spec compliance but awkward
// for `>= SeverityWarning` reads.  Invert the scale here so the
// caller's "warning or worse" check reads naturally.
func doctorSeverityRank(s output.Severity) int {
	// Severity in RFC 5424 runs 0..7 most-severe-first; invert so
	// higher == more severe for readable comparisons.
	return 7 - int(s)
}

// appendWALGapChecks walks each deployment with a configured
// repo and queries gapstate.LatestAny. A non-empty result
// becomes a walGapReport entry + a critical doctorIssue with a
// remediation Suggestion pointing at `pg_hardstorage repair
// slot`. The walk is read-only; failures to open a repo / list
// gap records are downgraded to warnings (the rest of the
// doctor report is still useful).
//
// Per-deployment scoping: each deployment's gaps live under
// wal/<deployment>/gaps/ in its OWN repo URL, so we walk by
// (deployment, repo) pair. Two deployments sharing one repo
// URL each get an independent gap query.
func appendWALGapChecks(ctx context.Context, cfg *config.LoadResult, issues []doctorIssue) ([]walGapReport, []doctorIssue) {
	type depRepo struct{ name, url string }
	var pairs []depRepo
	for name, dep := range cfg.Config.Deployments {
		if dep.Repo == "" {
			continue
		}
		pairs = append(pairs, depRepo{name: name, url: dep.Repo})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].name < pairs[j].name })

	var reports []walGapReport
	for _, p := range pairs {
		_, sp, err := repo.Open(ctx, p.url)
		if err != nil {
			// Repo unreachable was already surfaced by
			// appendRepoChecks; we skip without re-emitting.
			continue
		}
		// Defer Close in a closure so we don't hold N storage
		// plugins open across the loop.
		func() {
			defer sp.Close()
			rec, found, err := gapstate.New(sp).LatestAny(ctx, p.name)
			if err != nil {
				issues = append(issues, doctorIssue{
					Severity: output.SeverityWarning,
					Code:     "wal.gap_state_unreadable",
					Message:  fmt.Sprintf("doctor: read WAL-gap state for %s at %s: %v", p.name, p.url, err),
				})
				return
			}
			if !found {
				return
			}
			reports = append(reports, walGapReport{
				Deployment:  rec.Deployment,
				SlotName:    rec.SlotName,
				SlotRole:    rec.SlotRole,
				Timeline:    rec.Timeline,
				GapStartLSN: rec.GapStartLSN,
				GapEndLSN:   rec.GapEndLSN,
				GapBytes:    rec.GapBytes,
				DetectedAt:  rec.DetectedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			})
			issues = append(issues, doctorIssue{
				Severity: output.SeverityCritical,
				Code:     "wal.gap_persistent",
				Message: fmt.Sprintf("doctor: deployment %q has a persistent WAL gap of %d bytes on TLI %d (slot %q, detected %s); PITR within %s..%s is impossible from this repo",
					p.name, rec.GapBytes, rec.Timeline, rec.SlotName,
					rec.DetectedAt.Format("2006-01-02T15:04:05Z"),
					rec.GapStartLSN, rec.GapEndLSN),
				Suggestion: &output.Suggestion{
					Human:   "investigate via `pg_hardstorage repair slot <deployment>`; if the gap is small enough that re-bootstrapping from a fresh full backup is acceptable, take one to anchor a clean PITR window going forward. Once you've re-bootstrapped onto a new timeline the old gap's timeline is no longer live — `pg_hardstorage wal gap-purge <deployment> --orphans` then clears the superseded marker so this check stops re-alerting (a fresh backup alone does NOT clear it).",
					Command: "pg_hardstorage repair slot " + p.name,
					DocURL:  "https://docs.pghardstorage.org/runbooks/wal-gap-detected",
				},
			})
		}()
	}
	return reports, issues
}

// appendExpiredHoldChecks walks each deployment's holds and
// surfaces any whose ExpiresAt has passed. These are not a
// correctness concern — the soft-delete + cascade paths
// already skip them at decision time — but they're a useful
// operational hygiene signal: the marker is no longer
// protecting anything, and `hold list` will keep showing it
// until an operator runs `hold remove`. doctor surfaces them
// at SeverityNotice with a copy-pasteable cleanup Command.
//
// Per-deployment scoping: holds live under each deployment's
// manifests/ tree. We walk by (deployment, repo) pair and
// filter by ExpiresAt < now.
//
// Read-only: a repo we can't open is skipped silently
// (appendRepoChecks already surfaced the open error).
func appendExpiredHoldChecks(ctx context.Context, cfg *config.LoadResult, issues []doctorIssue) ([]expiredHoldReport, []doctorIssue) {
	type depRepo struct{ name, url string }
	var pairs []depRepo
	for name, dep := range cfg.Config.Deployments {
		if dep.Repo == "" {
			continue
		}
		pairs = append(pairs, depRepo{name: name, url: dep.Repo})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].name < pairs[j].name })

	now := time.Now().UTC()
	var reports []expiredHoldReport
	for _, p := range pairs {
		_, sp, err := repo.Open(ctx, p.url)
		if err != nil {
			continue // appendRepoChecks already surfaced
		}
		func() {
			defer sp.Close()
			store := backup.NewManifestStore(sp)
			holds, err := store.ListHolds(ctx, p.name)
			if err != nil {
				issues = append(issues, doctorIssue{
					Severity: output.SeverityWarning,
					Code:     "hold.list_failed",
					Message:  fmt.Sprintf("doctor: list holds for %s at %s: %v", p.name, p.url, err),
				})
				return
			}
			for _, h := range holds {
				if h.ExpiresAt == nil {
					continue // indefinite — never expires
				}
				if h.ActiveAt(now) {
					continue
				}
				reports = append(reports, expiredHoldReport{
					Deployment: h.Deployment,
					BackupID:   h.BackupID,
					Holder:     h.Holder,
					Reason:     h.Reason,
					HeldAt:     h.HeldAt.UTC().Format(time.RFC3339),
					ExpiredAt:  h.ExpiresAt.UTC().Format(time.RFC3339),
				})
			}
		}()
	}

	// Stable order by (deployment, backup_id) so doctor output
	// is deterministic across runs.
	sort.Slice(reports, func(i, j int) bool {
		if reports[i].Deployment != reports[j].Deployment {
			return reports[i].Deployment < reports[j].Deployment
		}
		return reports[i].BackupID < reports[j].BackupID
	})

	if len(reports) > 0 {
		// One Notice per `doctor` run with the count + (if ≤ 5)
		// the IDs in the message and a Suggestion whose
		// Command is the bulk `hold purge-expired` cleanup.
		// Single-shot `hold remove` still works for surgical
		// cases; doctor pushes the bulk path because that's
		// usually what an operator wants when confronted with
		// "you have N inert markers".
		message := fmt.Sprintf("doctor: %d expired hold marker(s) — operational cleanup signal, not a correctness issue", len(reports))
		if len(reports) <= 5 {
			ids := make([]string, 0, len(reports))
			for _, r := range reports {
				ids = append(ids, r.Deployment+"/"+r.BackupID)
			}
			message += " (" + strings.Join(ids, ", ") + ")"
		}
		issues = append(issues, doctorIssue{
			Severity: output.SeverityNotice,
			Code:     "hold.expired_present",
			Message:  message,
			Suggestion: &output.Suggestion{
				Human:   "the markers are inert — retention and `backup delete` already see past them. Run `hold purge-expired --dry-run` to preview, then `--yes` to clean up. Each removal is audit-emitted.",
				Command: "pg_hardstorage hold purge-expired --yes",
			},
		})
	}
	return reports, issues
}

// appendPGVersionChecks walks each deployment's latest manifest,
// records the PG major from the manifest, and surfaces a Notice
// when that major is outside pg.SupportedMajors(). This isn't a
// correctness problem — the wire protocol is stable across PG
// majors and pg_basebackup output is forward-compatible — but
// it means our integration suite doesn't cover that version
// and operators should track it as an unsupported configuration.
//
// Read-only; deployments without a recorded latest backup are
// skipped silently (they have nothing to report). A keystore
// without a verifier (fresh install) is also silent — the
// "no signing keypair" notice is the keystore probe's domain.
func appendPGVersionChecks(ctx context.Context, cfg *config.LoadResult, issues []doctorIssue) ([]pgVersionReport, []doctorIssue) {
	verifier, vErr := loadVerifier()
	if vErr != nil {
		// Verifier unavailable — silently skip. Other doctor
		// probes already surface the keystore problem.
		return nil, issues
	}

	type depRepo struct{ name, url string }
	var pairs []depRepo
	for name, dep := range cfg.Config.Deployments {
		if dep.Repo == "" {
			continue
		}
		pairs = append(pairs, depRepo{name: name, url: dep.Repo})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].name < pairs[j].name })

	reports := make([]pgVersionReport, 0, len(pairs))
	for _, p := range pairs {
		_, sp, err := repo.Open(ctx, p.url)
		if err != nil {
			continue
		}
		func() {
			defer sp.Close()
			store := backup.NewManifestStore(sp)
			// Pick the deployment's latest live manifest. A
			// deployment with no backups is silent — no row,
			// no issue (the "you have no backups" warning is
			// repoCheckReport's territory). Signature failures
			// are silently skipped, same posture as
			// pickLatestBackup in the verify command.
			var latest *backup.Manifest
			for m, mErr := range store.List(ctx, p.name, verifier) {
				if mErr != nil {
					continue
				}
				if m == nil {
					continue
				}
				if latest == nil || m.StoppedAt.After(latest.StoppedAt) {
					latest = m
				}
			}
			if latest == nil {
				return
			}
			major := latest.PGVersion / 10000
			if major <= 0 {
				return
			}
			supported := pg.IsSupportedMajor(major)
			reports = append(reports, pgVersionReport{
				Deployment:      p.name,
				PGMajor:         major,
				Supported:       supported,
				LatestBackupID:  latest.BackupID,
				LatestStoppedAt: latest.StoppedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			})
			if !supported {
				severity := output.SeverityNotice
				message := fmt.Sprintf("doctor: deployment %q runs on PG %d which is outside the tested support window [%d..%d]; the wire protocol works but the integration matrix doesn't cover this major",
					p.name, major, pg.MinSupportedMajor, pg.MaxSupportedMajor)
				issues = append(issues, doctorIssue{
					Severity: severity,
					Code:     "pg.unsupported_major",
					Message:  message,
					Suggestion: &output.Suggestion{
						Human: "operations continue to function — this is a coverage advisory, not a correctness gate. Track the PG version in your inventory and consider opening a request for the major to be added to the matrix.",
					},
				})
			}
		}()
	}
	return reports, issues
}

// doctorMaxManifestsVerified bounds the per-deployment manifest-signature
// walk so doctor stays fast on large repos. The walk is oldest-first
// (ManifestStore.List sorts by backup ID, which is chronological), which is
// exactly where a lost or rotated signing key leaves its orphaned backups —
// so the motivating failure is caught even when the walk is sampled.
// `repo check` remains the exhaustive surface.
const doctorMaxManifestsVerified = 256

// appendManifestSignatureChecks verifies each deployment's manifests against
// the current signing key and warns when any fail — most importantly when a
// manifest's embedded public key doesn't match the current keyring
// (ErrPublicKeyMismatch), which means the signing key was rotated or lost and
// those backups can no longer be restored or verified (#104). Before this,
// doctor reported `healthy: true` in exactly that situation because it only
// checked that *a* signing key exists, not that it matches the backups.
//
// Read-only; a keystore without a verifier is silent (the keystore probe
// owns the "no signing keypair" signal), as is a deployment with no backups.
func appendManifestSignatureChecks(ctx context.Context, cfg *config.LoadResult, issues []doctorIssue) ([]manifestSigReport, []doctorIssue) {
	verifier, vErr := loadVerifier()
	if vErr != nil {
		return nil, issues
	}

	type depRepo struct{ name, url string }
	var pairs []depRepo
	for name, dep := range cfg.Config.Deployments {
		if dep.Repo == "" {
			continue
		}
		pairs = append(pairs, depRepo{name: name, url: dep.Repo})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].name < pairs[j].name })

	var reports []manifestSigReport
	for _, p := range pairs {
		_, sp, err := repo.Open(ctx, p.url)
		if err != nil {
			continue // appendRepoChecks already surfaced reachability
		}
		func() {
			defer sp.Close()
			store := backup.NewManifestStore(sp)
			var checked, mismatched, failed int
			sampled := false
			for _, mErr := range store.List(ctx, p.name, verifier) {
				if checked >= doctorMaxManifestsVerified {
					sampled = true
					break
				}
				checked++
				switch {
				case mErr == nil:
					// verified clean
				case errors.Is(mErr, backup.ErrPublicKeyMismatch):
					mismatched++
				default:
					failed++
				}
			}
			if checked == 0 {
				return // no backups — nothing to verify
			}
			reports = append(reports, manifestSigReport{
				Deployment: p.name, Checked: checked,
				Mismatched: mismatched, Failed: failed, Sampled: sampled,
			})
			if mismatched > 0 {
				issues = append(issues, doctorIssue{
					Severity: output.SeverityWarning,
					Code:     "manifest_signature_mismatch",
					Message: fmt.Sprintf("doctor: deployment %q: %d of %d checked backup(s) were signed with a DIFFERENT key than the current keyring holds — they cannot be restored or verified. The signing key was likely rotated or lost (e.g. an ephemeral / non-persistent keyring).",
						p.name, mismatched, checked),
					Suggestion: &output.Suggestion{
						Human:   "the embedded public key in those manifests doesn't match your current signing key. If the original keypair still exists, restore it to the keyring; otherwise those backups are unrecoverable — take a fresh backup to anchor a clean window. `repo check` lists every affected manifest.",
						Command: "pg_hardstorage repo check --repo " + p.url,
					},
				})
			}
			if failed > 0 {
				issues = append(issues, doctorIssue{
					Severity: output.SeverityWarning,
					Code:     "manifest_signature_failures",
					Message: fmt.Sprintf("doctor: deployment %q: %d of %d checked manifest(s) failed verification (bad signature, malformed, or unreadable) — a restore against them would fail",
						p.name, failed, checked),
					Suggestion: &output.Suggestion{
						Human:   "investigate with `pg_hardstorage repo check` and `pg_hardstorage repair manifest`; check the audit chain for tampering.",
						Command: "pg_hardstorage repo check --repo " + p.url,
					},
				})
			}
		}()
	}
	return reports, issues
}

// appendRepoChecks walks the deployment config's unique repo URLs
// and appends a repoCheckReport per repo. Issues found during the
// walk are appended to the supplied slice. Idempotent + read-only.
func appendRepoChecks(ctx context.Context, cfg *config.LoadResult, issues []doctorIssue) ([]repoCheckReport, []doctorIssue) {
	seen := map[string]struct{}{}
	var urls []string
	for _, dep := range cfg.Config.Deployments {
		if dep.Repo == "" {
			continue
		}
		if _, ok := seen[dep.Repo]; ok {
			continue
		}
		seen[dep.Repo] = struct{}{}
		urls = append(urls, dep.Repo)
	}
	sort.Strings(urls)

	out := make([]repoCheckReport, 0, len(urls))
	for _, url := range urls {
		rep, repIssues := checkOneRepo(ctx, url)
		out = append(out, rep)
		issues = append(issues, repIssues...)
	}
	return out, issues
}

// drillStatusReport summarises one deployment's recovery-drill
// freshness — the continuous "is the latest backup provably
// restorable?" probe. A backup that has never been restored is
// unproven; drills turn exit-0 backups into demonstrated restores,
// and this report is how an operator sees that proof go stale.
type drillStatusReport struct {
	Deployment  string `json:"deployment"`
	Repo        string `json:"repo"`
	LastVerdict string `json:"last_verdict,omitempty"`
	LastAt      string `json:"last_at,omitempty"`
	LastPassAt  string `json:"last_pass_at,omitempty"`
	Fresh       bool   `json:"fresh"`
}

// defaultDrillMaxAge is how old the last SUCCESSFUL drill may be
// before doctor escalates. One week matches a weekly drill schedule
// with a day of slack.
const defaultDrillMaxAge = 7 * 24 * time.Hour

// appendDrillChecks reads each deployment's drill history
// (recovery/drills/ in its repo) and flags:
//
//   - recovery.drill_never_run  (notice)  — no drill has ever run;
//     the backups are unproven. Suggests scheduling one.
//   - recovery.drill_failing    (CRITICAL) — the most recent drill
//     did not pass: the latest backup could not be proven restorable.
//   - recovery.drill_stale      (CRITICAL) — the last PASSING drill
//     is older than maxAge: restorability is no longer being proven.
func appendDrillChecks(ctx context.Context, cfg *config.LoadResult, maxAge time.Duration, issues []doctorIssue) ([]drillStatusReport, []doctorIssue) {
	if maxAge <= 0 {
		maxAge = defaultDrillMaxAge
	}
	names := make([]string, 0, len(cfg.Config.Deployments))
	for name := range cfg.Config.Deployments {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []drillStatusReport
	now := time.Now().UTC()
	for _, name := range names {
		dep := cfg.Config.Deployments[name]
		if dep.Repo == "" {
			continue
		}
		_, sp, err := repo.Open(ctx, dep.Repo)
		if err != nil {
			continue // repo.unreachable already surfaced by appendRepoChecks
		}
		entries, herr := recovery.NewHistoryStore(sp).List(ctx, recovery.HistoryFilter{
			Deployment: name,
			Reverse:    true,
			Limit:      50,
		})
		sp.Close()
		if herr != nil {
			continue // best-effort, like the other doctor probes
		}
		rep := drillStatusReport{Deployment: name, Repo: dep.Repo}
		if len(entries) == 0 {
			out = append(out, rep)
			issues = append(issues, doctorIssue{
				Severity: output.SeverityNotice,
				Code:     "recovery.drill_never_run",
				Message:  fmt.Sprintf("doctor: deployment %q has never run a recovery drill — its backups are unproven (a backup that has never been restored is not known to be restorable)", name),
				Suggestion: &output.Suggestion{
					Human:   fmt.Sprintf("schedule a periodic drill: `pg_hardstorage schedule %s \"daily_at 03:00\" --task drill` (the agent runs it), or run one now with `pg_hardstorage recovery drill %s --repo %s`", name, name, dep.Repo),
					Command: fmt.Sprintf("pg_hardstorage recovery drill %s --repo %s", name, dep.Repo),
				},
			})
			continue
		}
		latest := entries[0]
		rep.LastVerdict = string(latest.Verdict)
		rep.LastAt = latest.GeneratedAt.UTC().Format(time.RFC3339)
		var lastPass *recovery.DrillHistoryEntry
		for _, e := range entries {
			if e.Verdict == recovery.DrillVerdictPass {
				lastPass = e
				break
			}
		}
		if lastPass != nil {
			rep.LastPassAt = lastPass.GeneratedAt.UTC().Format(time.RFC3339)
		}
		if latest.Verdict != recovery.DrillVerdictPass {
			issues = append(issues, doctorIssue{
				Severity: output.SeverityCritical,
				Code:     "recovery.drill_failing",
				Message:  fmt.Sprintf("doctor: the most recent recovery drill for %q FAILED (verdict %q at %s, backup %s) — the latest backup could not be proven restorable", name, latest.Verdict, rep.LastAt, latest.BackupID),
				Suggestion: &output.Suggestion{
					Human:   "investigate immediately: run the drill by hand with --keep-target-dir and inspect; a failing drill means a restore during a real incident would likely fail the same way",
					Command: fmt.Sprintf("pg_hardstorage recovery drill %s --repo %s", name, dep.Repo),
				},
			})
			out = append(out, rep)
			continue
		}
		if now.Sub(lastPass.GeneratedAt) > maxAge {
			issues = append(issues, doctorIssue{
				Severity: output.SeverityCritical,
				Code:     "recovery.drill_stale",
				Message:  fmt.Sprintf("doctor: last successful recovery drill for %q was %s (> %s ago) — restorability is no longer being proven", name, rep.LastPassAt, maxAge),
				Suggestion: &output.Suggestion{
					Human:   fmt.Sprintf("check the agent's drill schedule (`pg_hardstorage schedule %s --task drill`) and the agent's health; or run a drill now", name),
					Command: fmt.Sprintf("pg_hardstorage recovery drill %s --repo %s", name, dep.Repo),
				},
			})
			out = append(out, rep)
			continue
		}
		rep.Fresh = true
		out = append(out, rep)
	}
	return out, issues
}

// checkOneRepo runs the read-only health probes against one
// repo URL. Errors at any stage are captured in the report's
// fields + surfaced via doctorIssue entries; the function never
// returns an error.
func checkOneRepo(ctx context.Context, url string) (repoCheckReport, []doctorIssue) {
	r := repoCheckReport{URL: url}
	var issues []doctorIssue

	repoMeta, sp, err := repo.Open(ctx, url)
	if err != nil {
		r.OpenError = err.Error()
		issues = append(issues, doctorIssue{
			Severity: output.SeverityError,
			Code:     "repo.unreachable",
			Message:  fmt.Sprintf("doctor: open repo %s: %v", url, err),
			Suggestion: &output.Suggestion{
				Human: "verify the storage backend is reachable and the operator's credentials are still valid",
			},
		})
		return r, issues
	}
	defer sp.Close()
	r.Reachable = true

	// Audit chain length + anchor freshness. Both are best-effort:
	// a brand-new repo with no events and no anchor is healthy
	// (AnchorFresh=true, ChainEventCount=0). A repo with N events
	// and no anchor surfaces "anchor stale" as a warning so an
	// operator running doctor under cron sees the gap.
	chainKeys := 0
	for info, err := range sp.List(ctx, "audit/") {
		if err != nil {
			issues = append(issues, doctorIssue{
				Severity: output.SeverityWarning,
				Code:     "audit.chain_unreadable",
				Message:  fmt.Sprintf("doctor: read audit chain at %s: %v", url, err),
			})
			return r, issues
		}
		// Count chain events only; skip the head pointer + every
		// anchor record. Same filter as audit.Store.allKeysSorted.
		if !strings.HasSuffix(info.Key, ".json") {
			continue
		}
		if info.Key == audit.HeadKey {
			continue
		}
		if strings.HasPrefix(info.Key, audit.AnchorPrefix) {
			continue
		}
		chainKeys++
	}
	r.ChainEventCount = chainKeys

	log := audit.NewStorageBackedLogWithRetention(sp, repoMeta.WORM)
	latest, err := log.LatestAnchor(ctx)
	if err != nil {
		issues = append(issues, doctorIssue{
			Severity: output.SeverityWarning,
			Code:     "audit.anchor_unreadable",
			Message:  fmt.Sprintf("doctor: read latest anchor at %s: %v", url, err),
		})
		return r, issues
	}
	if latest == nil {
		// No anchor at all. Empty chain = healthy; non-empty chain
		// = stale anchor + actionable suggestion.
		if chainKeys == 0 {
			r.AnchorFresh = true
			return r, issues
		}
		issues = append(issues, doctorIssue{
			Severity: output.SeverityWarning,
			Code:     "audit.anchor_missing",
			Message:  fmt.Sprintf("doctor: %d audit event(s) at %s but no transparency-log anchor; run `pg_hardstorage audit anchor --repo %s`", chainKeys, url, url),
			Suggestion: &output.Suggestion{
				Human:   fmt.Sprintf("run `pg_hardstorage audit anchor --repo %s` (or wire periodic anchoring via the agent's audit_anchor schedule)", url),
				Command: "pg_hardstorage audit anchor --repo " + url,
			},
		})
		return r, issues
	}

	r.HasLatestAnchor = true
	r.LatestAnchorSeq = latest.HeadSequence
	// Freshness compares the anchor's head sequence against the CURRENT
	// head sequence of the SAME chain the anchor witnesses, read from that
	// shard's head pointer. We must NOT derive the head sequence by
	// counting event files (the old `chainKeys-1 == headSequence` test):
	//   - under WORM retention the oldest events are pruned while sequence
	//     numbers keep climbing, so on an aged repo chainKeys <
	//     headSequence+1 and a perfectly fresh anchor was reported stale
	//     with a nonsensical negative "behind" count — which re-anchoring
	//     could never fix because the count never catches up; and
	//   - chainKeys counts events across ALL shards while an anchor
	//     witnesses ONE shard, so any multi-shard repo over-counted and
	//     reported a fresh anchor as dozens of events behind.
	// The head pointer is a perf cache that is NOT subject to retention,
	// so its Sequence is the authoritative current head for its shard.
	headSeq, ok := readAuditHeadSequence(ctx, sp, latest.Shard)
	if !ok {
		// No readable head pointer — judging freshness here would mean
		// falling back to the retention-truncated count, which is exactly
		// the false positive we're avoiding. Treat the present anchor as
		// fresh rather than emit a guess.
		r.AnchorFresh = true
		return r, issues
	}
	if latest.HeadSequence >= headSeq {
		r.AnchorFresh = true
		return r, issues
	}
	r.AnchorBehindEvents = int(headSeq - latest.HeadSequence)
	issues = append(issues, doctorIssue{
		Severity: output.SeverityNotice,
		Code:     "audit.anchor_stale",
		Message: fmt.Sprintf("doctor: anchor for %s is %d event(s) behind (latest anchor at sequence %d, chain head at sequence %d)",
			url, r.AnchorBehindEvents, latest.HeadSequence, headSeq),
		Suggestion: &output.Suggestion{
			Human:   fmt.Sprintf("run `pg_hardstorage audit anchor --repo %s` to refresh the anchor", url),
			Command: "pg_hardstorage audit anchor --repo " + url,
		},
	})
	return r, issues
}

// readAuditHeadSequence reads the head pointer for a given audit shard
// (empty = the global chain) and returns its Sequence — the authoritative
// current head-event sequence for that chain. Unlike counting event
// files, this is stable under WORM retention pruning (the head pointer is
// a perf cache that retention deliberately leaves alone) and is scoped to
// the one shard the caller's anchor witnesses. Returns ok=false when the
// pointer is absent, unreadable, or carries an unexpected schema, so the
// caller can avoid emitting a freshness verdict it can't substantiate.
func readAuditHeadSequence(ctx context.Context, sp storage.StoragePlugin, shard string) (int64, bool) {
	rc, err := sp.Get(ctx, audit.HeadKeyForShard(shard))
	if err != nil {
		return 0, false
	}
	defer rc.Close()
	var hp audit.HeadPointer
	if err := json.NewDecoder(rc).Decode(&hp); err != nil {
		return 0, false
	}
	if hp.Schema != audit.HeadPointerSchema {
		return 0, false
	}
	return hp.Sequence, true
}

// doctorReport is the typed body. Stable JSON shape under pg_hardstorage.v1.
type doctorReport struct {
	Mode         string              `json:"mode"`
	Paths        []pathReport        `json:"paths"`
	Config       configReport        `json:"config"`
	Keystore     keystoreReport      `json:"keystore"`
	Airgap       airgapReport        `json:"airgap"`
	Repos        []repoCheckReport   `json:"repos,omitempty"`
	WALGaps      []walGapReport      `json:"wal_gaps,omitempty"`
	ExpiredHolds []expiredHoldReport `json:"expired_holds,omitempty"`
	PGVersions   []pgVersionReport   `json:"pg_versions,omitempty"`
	ManifestSig  []manifestSigReport `json:"manifest_signatures,omitempty"`
	Drills       []drillStatusReport `json:"drills,omitempty"`
	Issues       []doctorIssue       `json:"issues,omitempty"`
	Healthy      bool                `json:"healthy"`
}

// manifestSigReport summarises whether a deployment's manifests verify
// against the current signing key. The motivating failure (#103/#104) is a
// lost or rotated keyring: backups signed with the old key fail
// ParseAndVerify with ErrPublicKeyMismatch and become unrestorable, while
// `doctor` previously reported healthy because the (new) key still exists.
type manifestSigReport struct {
	Deployment string `json:"deployment"`
	Checked    int    `json:"checked"`
	Mismatched int    `json:"key_mismatch"`      // ErrPublicKeyMismatch — wrong signing key
	Failed     int    `json:"other_failures"`    // other verification / read failures
	Sampled    bool   `json:"sampled,omitempty"` // true when the walk hit the cap
}

// airgapReport surfaces the active air-gap policy plus a per-
// configured-sink view of "would this URL pass the gate?".  When
// Mode is "off" the section's job is to confirm the operator is
// running unrestricted; when Mode is "strict" the per-sink rows
// are the operator's "is anything misconfigured?" diagnostic.
type airgapReport struct {
	Mode      string             `json:"mode"`
	Allowlist []string           `json:"allowlist,omitempty"`
	Sinks     []airgapSinkReport `json:"sinks,omitempty"`
}

type airgapSinkReport struct {
	Name    string `json:"name"`
	Plugin  string `json:"plugin"`
	URL     string `json:"url,omitempty"`
	Allowed bool   `json:"allowed"`
	Refusal string `json:"refusal,omitempty"`
}

// pgVersionReport summarises the PG major version observed for
// each deployment with backups in the repo. Surfaces:
//   - the manifest-derived major (so a+ "you upgraded the
//     server but didn't take a fresh backup" check can land)
//   - whether the major is in pg.SupportedMajors() — operators
//     running PG < MinSupportedMajor or > MaxSupportedMajor see
//     a Notice that the wire protocol works but the integration
//     suite doesn't cover their version.
type pgVersionReport struct {
	Deployment string `json:"deployment"`
	PGMajor    int    `json:"pg_major"`
	Supported  bool   `json:"supported"`
	// LatestBackupID + LatestStoppedAt make the report
	// self-describing — operators see which backup the
	// version reading came from without re-running list/show.
	LatestBackupID  string `json:"latest_backup_id,omitempty"`
	LatestStoppedAt string `json:"latest_stopped_at,omitempty"`
}

// expiredHoldReport surfaces a hold whose ExpiresAt is in the
// past — the marker no longer protects the manifest from
// deletion (retention will reap; SoftDelete proceeds), but the
// marker is still on disk. Operators should clean these up via
// `hold remove` so `hold list` doesn't accumulate stale
// entries.
//
// This is an OPERATIONAL hygiene signal, not a correctness
// problem — emitted at SeverityNotice, never escalated.
type expiredHoldReport struct {
	Deployment string `json:"deployment"`
	BackupID   string `json:"backup_id"`
	Holder     string `json:"holder,omitempty"`
	Reason     string `json:"reason,omitempty"`
	HeldAt     string `json:"held_at"`
	ExpiredAt  string `json:"expired_at"`
}

// walGapReport summarises the latest WAL gap recorded for a
// deployment. Empty WALGaps in the doctor report means no gaps
// have been persisted (either no failovers happened, or every
// failover landed on Mechanism A/B with cluster-recreated slots
// → no actual gap).
type walGapReport struct {
	Deployment  string `json:"deployment"`
	SlotName    string `json:"slot_name"`
	SlotRole    string `json:"slot_role,omitempty"`
	Timeline    uint32 `json:"timeline"`
	GapStartLSN string `json:"gap_start_lsn"`
	GapEndLSN   string `json:"gap_end_lsn"`
	GapBytes    uint64 `json:"gap_bytes"`
	DetectedAt  string `json:"detected_at"`
}

// repoCheckReport summarises one repository's health from the
// doctor's read-only perspective. covers reachability + audit
// chain anchor freshness; future checks (slot lag, scrub freshness,
// approval-request stales) accrete here.
//
// Fields are populated only when the relevant probe ran — a repo
// that's unreachable has Reachable=false and the rest of the
// fields are zero-valued.
type repoCheckReport struct {
	URL                string `json:"url"`
	Reachable          bool   `json:"reachable"`
	OpenError          string `json:"open_error,omitempty"`
	ChainEventCount    int    `json:"chain_event_count"`
	HasLatestAnchor    bool   `json:"has_latest_anchor"`
	LatestAnchorSeq    int64  `json:"latest_anchor_sequence,omitempty"`
	AnchorBehindEvents int    `json:"anchor_behind_events,omitempty"`
	AnchorFresh        bool   `json:"anchor_fresh"`
}

type pathReport struct {
	Domain  string `json:"domain"`
	Label   string `json:"-"`
	Value   string `json:"value"`
	Source  string `json:"source"`
	Reason  string `json:"reason,omitempty"`
	Exists  bool   `json:"exists"`
	IsDir   bool   `json:"is_dir,omitempty"`
	ModeStr string `json:"mode,omitempty"`
	StatErr string `json:"stat_error,omitempty"`
}

type configReport struct {
	Configured      bool               `json:"configured"`
	Schema          string             `json:"schema,omitempty"`
	PathsRoot       string             `json:"paths_root,omitempty"`
	LLMProvider     string             `json:"llm_provider,omitempty"`
	SourceFiles     []sourceFileReport `json:"source_files"`
	LoadError       string             `json:"load_error,omitempty"`
	DeploymentCount int                `json:"deployment_count"`
	DeploymentNames []string           `json:"deployment_names,omitempty"`
	// Classifications maps deployment-name → effective classification
	// (operator-set or implicit "internal"). Surfaces compliance posture
	// at a glance for the operator running `doctor` and for monitoring
	// tools parsing the JSON.
	Classifications map[string]string `json:"classifications,omitempty"`
	SinkCount       int               `json:"sink_count"`
}

// keystoreReport summarises what the doctor sees in the keyring
// directory. Presence + mode for the signing key and the KEK; the
// keys themselves are NEVER read or printed.
type keystoreReport struct {
	KeyringDir       string `json:"keyring_dir"`
	SigningKeyExists bool   `json:"signing_key_exists"`
	KEKExists        bool   `json:"kek_exists"`
}

type sourceFileReport struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	ReadOK   bool   `json:"read_ok"`
	ParseErr string `json:"parse_error,omitempty"`
}

type doctorIssue struct {
	Severity   output.Severity    `json:"severity"`
	Code       string             `json:"code"`
	Message    string             `json:"message"`
	Suggestion *output.Suggestion `json:"suggestion,omitempty"`
}

func buildDoctorReport(p *paths.Paths, cfg *config.LoadResult, cfgErr error) doctorReport {
	r := doctorReport{Mode: p.ModeName, Healthy: true}

	for _, dp := range p.All() {
		pr := pathReport{
			Domain: dp.Domain,
			Label:  dp.Label,
			Value:  dp.Path.Value,
			Source: string(dp.Path.Source),
			Reason: dp.Path.Reason,
		}
		if info, err := os.Stat(dp.Path.Value); err == nil {
			pr.Exists = true
			pr.IsDir = info.IsDir()
			pr.ModeStr = info.Mode().String()
		} else if !errors.Is(err, os.ErrNotExist) {
			pr.StatErr = err.Error()
		}
		r.Paths = append(r.Paths, pr)
	}

	if cfg != nil {
		r.Config.Configured = cfg.IsConfigured()
		r.Config.Schema = cfg.Config.Schema
		r.Config.PathsRoot = cfg.Config.Paths.Root
		r.Config.LLMProvider = cfg.Config.LLM.Provider
		r.Config.SinkCount = len(cfg.Config.Sinks)
		r.Config.DeploymentCount = len(cfg.Config.Deployments)
		if len(cfg.Config.Deployments) > 0 {
			r.Config.Classifications = make(map[string]string, len(cfg.Config.Deployments))
		}
		for name, dep := range cfg.Config.Deployments {
			r.Config.DeploymentNames = append(r.Config.DeploymentNames, name)
			r.Config.Classifications[name] = effectiveClassification(dep.Classification)
		}
		sort.Strings(r.Config.DeploymentNames)
		for _, sf := range cfg.SourceFiles {
			r.Config.SourceFiles = append(r.Config.SourceFiles, sourceFileReport{
				Path: sf.Path, Kind: sf.Kind, ReadOK: sf.ReadOK, ParseErr: sf.ParseErr,
			})
		}
	}

	// Keystore report. We check for file presence; the keys themselves
	// are NEVER read here. Operators paranoid about doctor leaking key
	// material can verify by reading the source.
	r.Keystore.KeyringDir = p.Keyring.Value
	// The signing keypair filename has lived at
	// `manifest_signing.ed25519` for the whole lifetime of the
	// `keystore` package (see keystore.PrivateKeyFile).  The earlier
	// probe here checked for `ed25519.priv`, an obsolete name that
	// never matched a real keystore on disk — so `doctor` always
	// reported the signing key as absent even right after a
	// successful `pg_hardstorage init`.  Issue #15.
	r.Keystore.SigningKeyExists = fileExists(filepath.Join(p.Keyring.Value, keystore.PrivateKeyFile))
	r.Keystore.KEKExists = keystore.KEKExists(p.Keyring.Value)
	if !r.Keystore.SigningKeyExists {
		// Not a fatal issue — the runner generates a keypair on first
		// run — but worth surfacing so an operator inspecting a
		// fresh-clone install knows the next step.
		r.Issues = append(r.Issues, doctorIssue{
			Severity: output.SeverityNotice,
			Code:     "keystore.signing_key_absent",
			Message:  "no signing keypair at " + r.Keystore.KeyringDir + " — backups will generate one on first run",
			Suggestion: &output.Suggestion{
				Human:   "run `pg_hardstorage init` to provision the keyring + KEK + signing keypair",
				Command: "pg_hardstorage init",
			},
		})
	}
	if cfgErr != nil {
		r.Config.LoadError = cfgErr.Error()
		r.Issues = append(r.Issues, doctorIssue{
			Severity: output.SeverityError,
			Code:     "config.invalid",
			Message:  cfgErr.Error(),
			Suggestion: &output.Suggestion{
				Human:  "fix or remove the offending config file; see the path above",
				DocURL: "docs/SPEC.md",
			},
		})
		r.Healthy = false
	}

	// Air-gap section. Reports the resolved policy plus a per-sink
	// "would this URL pass?" preview, so an operator running
	// `--airgapped` sees at a glance which sinks would be refused
	// before they even try to flush an event.
	r.Airgap = buildAirgapReport(cfg)
	for _, s := range r.Airgap.Sinks {
		if !s.Allowed {
			r.Issues = append(r.Issues, doctorIssue{
				Severity: output.SeverityWarning,
				Code:     "airgap.sink_refused",
				Message:  fmt.Sprintf("sink %q (%s): air-gap policy refuses %s", s.Name, s.Plugin, s.URL),
				Suggestion: &output.Suggestion{
					Human:  fmt.Sprintf("either move sink %q to an in-perimeter URL, or add the host to airgap.allowlist in pg_hardstorage.yaml", s.Name),
					DocURL: "docs/SPEC.md#airgap",
				},
			})
			r.Healthy = false
		}
	}

	// Fresh-install hint: not an Issue (non-blocking), surfaced in text only.
	// JSON consumers use Configured=false to detect the same state.

	return r
}

// buildAirgapReport collects the active policy and runs each
// configured sink's URL-bearing field through EndpointAllowed.
// Helpers below pick the right config key per plugin (slack:
// webhook_url, jira: base_url, ...).  Sinks with no URL config
// (syslog, email when local-only) are reported with Allowed=true
// and an empty URL so the report shows the full sink count.
func buildAirgapReport(cfg *config.LoadResult) airgapReport {
	pol := airgap.Default()
	r := airgapReport{
		Mode:      pol.Mode.String(),
		Allowlist: append([]string(nil), pol.Allowlist...),
	}
	if cfg == nil {
		return r
	}
	for _, spec := range cfg.Config.Sinks {
		row := airgapSinkReport{
			Name:    spec.Name,
			Plugin:  spec.Plugin,
			URL:     extractSinkURL(spec),
			Allowed: true,
		}
		if row.URL != "" {
			if err := pol.EndpointAllowed(row.URL); err != nil {
				row.Allowed = false
				row.Refusal = err.Error()
			}
		}
		r.Sinks = append(r.Sinks, row)
	}
	return r
}

// extractSinkURL pulls the URL-bearing field out of a sink spec
// without instantiating the sink. Mapped per plugin name; an
// unknown plugin returns "" (which the doctor reports as
// "no URL — gate doesn't apply").
func extractSinkURL(spec output.SinkSpec) string {
	get := func(key string) string {
		v, _ := spec.Config[key].(string)
		return v
	}
	switch spec.Plugin {
	case "slack":
		return get("webhook_url")
	case "webhook":
		return get("url")
	case "jira":
		return get("base_url")
	case "servicenow":
		return get("instance_url")
	case "opsgenie":
		if u := get("api_url"); u != "" {
			return u
		}
		return "https://api.opsgenie.com" // matches DefaultAPIURL
	case "pagerduty":
		// PagerDuty's URL is hard-coded — the spec's gate is
		// always against the canonical Events API URL.
		return "https://events.pagerduty.com/v2/enqueue"
	}
	return ""
}

// WriteText renders the doctor report for the text renderer. Compact,
// scannable, reads top-to-bottom.
func (r doctorReport) WriteText(w io.Writer) error {
	bw := &bufWriter{w: w}
	bw.printf("Mode: %s\n", r.Mode)
	bw.printf("\nPATHS\n")
	for _, pr := range r.Paths {
		state := "missing"
		if pr.Exists {
			state = "exists"
			if pr.ModeStr != "" {
				state += " " + pr.ModeStr
			}
		} else if pr.StatErr != "" {
			state = "stat-error: " + pr.StatErr
		}
		// Padding to keep columns roughly aligned for typical paths.
		label := fmt.Sprintf("%-14s", pr.Label)
		value := fmt.Sprintf("%-50s", pr.Value)
		bw.printf("  %s %s [%s] %s\n", label, value, pr.Source, state)
	}

	bw.printf("\nCONFIG\n")
	if r.Config.Configured {
		bw.printf("  Status:        configured\n")
		if r.Config.Schema != "" {
			bw.printf("  Schema:        %s\n", r.Config.Schema)
		}
		if r.Config.PathsRoot != "" {
			bw.printf("  paths.root:    %s\n", r.Config.PathsRoot)
		}
		if r.Config.LLMProvider != "" {
			bw.printf("  LLM provider:  %s\n", r.Config.LLMProvider)
		}
		bw.printf("  Deployments:   %d", r.Config.DeploymentCount)
		if len(r.Config.DeploymentNames) > 0 {
			bw.printf(" (%s)", strings.Join(r.Config.DeploymentNames, ", "))
		}
		bw.printf("\n")
		if len(r.Config.Classifications) > 0 {
			// Per-deployment classification line — keeps the doctor
			// scan-friendly without requiring a separate command for
			// "what's my compliance posture?"
			for _, name := range r.Config.DeploymentNames {
				bw.printf("    %-20s class: %s\n", name, r.Config.Classifications[name])
			}
		}
		bw.printf("  Sinks:         %d\n", r.Config.SinkCount)
	} else {
		bw.printf("  Status:        not yet configured\n")
		bw.printf("  Suggested:     pg_hardstorage init\n")
	}
	if r.Config.LoadError != "" {
		bw.printf("  Load error:    %s\n", r.Config.LoadError)
	}
	if len(r.Config.SourceFiles) > 0 {
		bw.printf("  Source files:\n")
		for _, sf := range r.Config.SourceFiles {
			tag := "loaded"
			if !sf.ReadOK {
				tag = "absent"
			}
			if sf.ParseErr != "" {
				tag = "parse-error: " + sf.ParseErr
			}
			bw.printf("    [%-7s] %s\n", tag, sf.Path)
		}
	}

	bw.printf("\nKEYSTORE\n")
	bw.printf("  Dir:           %s\n", r.Keystore.KeyringDir)
	bw.printf("  Signing key:   %s\n", presenceLabel(r.Keystore.SigningKeyExists))
	bw.printf("  KEK:           %s", presenceLabel(r.Keystore.KEKExists))
	if r.Keystore.KEKExists {
		bw.printf(" (encryption ON by default for new backups)\n")
	} else {
		bw.printf(" (backups will be unencrypted; run `init --encrypt` to provision)\n")
	}

	bw.printf("\nAIRGAP\n")
	bw.printf("  Mode:          %s\n", r.Airgap.Mode)
	if len(r.Airgap.Allowlist) > 0 {
		bw.printf("  Allowlist:     %s\n", strings.Join(r.Airgap.Allowlist, ", "))
	}
	if len(r.Airgap.Sinks) > 0 {
		for _, s := range r.Airgap.Sinks {
			marker := "✓"
			if !s.Allowed {
				marker = "✗"
			}
			line := fmt.Sprintf("  %s %s (%s)", marker, s.Name, s.Plugin)
			if s.URL != "" {
				line += " — " + s.URL
			}
			bw.printf("%s\n", line)
			if !s.Allowed {
				bw.printf("    refused: %s\n", s.Refusal)
			}
		}
	}

	if len(r.Repos) > 0 {
		bw.printf("\nREPOS\n")
		for _, rr := range r.Repos {
			state := "unreachable"
			if rr.Reachable {
				state = "reachable"
			}
			bw.printf("  %s — %s\n", rr.URL, state)
			if !rr.Reachable {
				if rr.OpenError != "" {
					bw.printf("    error: %s\n", rr.OpenError)
				}
				continue
			}
			bw.printf("    audit chain: %d event(s)\n", rr.ChainEventCount)
			switch {
			case !rr.HasLatestAnchor && rr.ChainEventCount == 0:
				bw.printf("    anchor: none (chain empty — fresh repo, healthy)\n")
			case !rr.HasLatestAnchor:
				bw.printf("    anchor: ✗ NONE (chain has events, no transparency anchor)\n")
			case rr.AnchorFresh:
				bw.printf("    anchor: ✓ fresh (sequence %d)\n", rr.LatestAnchorSeq)
			default:
				bw.printf("    anchor: ⚠ stale — %d event(s) behind (latest anchor at sequence %d)\n",
					rr.AnchorBehindEvents, rr.LatestAnchorSeq)
			}
		}
	}

	if len(r.ExpiredHolds) > 0 {
		bw.printf("\nEXPIRED HOLDS (%d)\n", len(r.ExpiredHolds))
		for _, h := range r.ExpiredHolds {
			bw.printf("  %s/%s — expired %s", h.Deployment, h.BackupID, h.ExpiredAt)
			if h.Holder != "" {
				bw.printf(" (holder=%s)", h.Holder)
			}
			if h.Reason != "" {
				bw.printf(" — %s", h.Reason)
			}
			bw.printf("\n")
		}
	}

	if len(r.Issues) > 0 {
		bw.printf("\nISSUES\n")
		for _, is := range r.Issues {
			bw.printf("  [%s] %s: %s\n", strings.ToUpper(is.Severity.String()), is.Code, is.Message)
			if is.Suggestion != nil && is.Suggestion.Human != "" {
				bw.printf("    hint: %s\n", is.Suggestion.Human)
			}
		}
	} else {
		bw.printf("\nSummary: ")
		if r.Healthy && r.Config.Configured {
			bw.printf("\u2713 healthy\n")
		} else if r.Healthy {
			bw.printf("not yet configured (run pg_hardstorage init)\n")
		} else {
			bw.printf("not healthy\n")
		}
	}
	return bw.err
}

// fileExists is a tiny os.Stat wrapper. We use it only for presence
// checks — never to read or report file contents.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// presenceLabel renders "✓ present" / "✗ absent" — the human-friendly
// presence indicator the keystore + path sections share.
func presenceLabel(ok bool) string {
	if ok {
		return "✓ present"
	}
	return "✗ absent"
}

// bufWriter is a tiny error-collecting Fprintf wrapper. It lets us write
// linear code in WriteText without re-checking err on every Fprintf.
type bufWriter struct {
	w   io.Writer
	err error
}

func (b *bufWriter) printf(format string, args ...any) {
	if b.err != nil {
		return
	}
	_, b.err = fmt.Fprintf(b.w, format, args...)
}
