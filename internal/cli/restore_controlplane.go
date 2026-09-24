// restore_controlplane.go — '--control-plane' mode of `restore`: POST + poll a remote-agent restore job.
package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// runRestoreControlPlane is the --control-plane mode of `pg_hardstorage
// restore`. Instead of running the restore in-process, it POSTs the
// request to the control plane and polls /v1/jobs/<id> until the job
// reaches a terminal state. Progress events are forwarded through the
// local dispatcher so the operator's terminal looks the same as if
// the restore ran locally.
//
// This is the operator-side complement to's restore-via-control-
// plane work: the server-side route (handleEnqueueRestore) and the
// agent-side executor (RestoreExecutor) shipped first; this is the
// CLI that wires them together.
//
// Issue #99 note (PITR reachability): the CLI side cannot run the
// reachability gate (CheckTargetReachable) here because it does not
// have the manifest — that requires opening the repo, which is the
// remote agent's responsibility in control-plane mode. The agent's
// in-process restore.Restore() runs the gate as defence-in-depth, so
// an unreachable --to-lsn surfaces as a structured
// restore.target_unreachable error from the agent (routed to
// ExitConflict, same as the local path).  Operators who want pre-
// flight verification before the round-trip can run
// `pg_hardstorage restore <deployment> <backup> --preview --to-lsn X
// --repo <url>` LOCALLY against the same repo URL the agent uses —
// preview only reads, so it doesn't conflict with the eventual
// control-plane restore.
func runRestoreControlPlane(cmd *cobra.Command, opts restoreOpts) error {
	d := DispatcherFrom(cmd)

	// --preview promises "plan the restore but do not write anything".
	// There is no remote preview: the only thing this path can do is
	// enqueue a real restore (with allow_overwrite when --force is set),
	// so honouring the flag by ignoring it would execute exactly what
	// the operator asked NOT to. Refuse; preview locally against the
	// agent's repo instead (it only reads).
	if opts.preview {
		return output.NewError("usage.unsupported_flag",
			"restore --control-plane: --preview cannot be dispatched (the control plane would run a REAL restore); run `pg_hardstorage restore <deployment> <backup> --preview --repo <url>` locally against the agent's repository instead").
			Wrap(output.ErrUsage)
	}
	// Flags the agent's RestoreExecutor has no way to honour. Each is
	// refused rather than dropped: an operator who asked for a
	// threshold-attestation gate or a cluster-start smoke test and got
	// a restore without one has been told something false.
	if err := refuseUnsupportedControlPlaneFlags(cmd, "restore", map[string]string{
		"require-threshold-attestation": "the agent has no threshold roster; verify the attestation locally first",
		"verify-restore":                "the cluster-start smoke test runs only in a local restore",
		"kms-config":                    "cloud-KMS settings come from the agent's own config (kms.providers)",
		"chain-staging-root":            "chain staging lives on the agent host and is not remotely configurable",
		"reset-chain-staging":           "chain staging lives on the agent host and is not remotely configurable",
	}); err != nil {
		return err
	}

	// Required-field validation. We mirror the server-side checks at
	// the CLI boundary too so the operator gets a clear local error
	// rather than a 400 from the server.
	if opts.targetDir == "" {
		return output.NewError("usage.missing_flag",
			"restore: --target is required (control-plane mode)").Wrap(output.ErrUsage)
	}
	// Validate one-target rule before we send so the server doesn't
	// have to refuse a malformed body. (The agent-side executor
	// validates again as a defence-in-depth.)
	count := 0
	if opts.toLSN != "" {
		count++
	}
	if opts.toTime != "" {
		count++
	}
	if opts.toName != "" {
		count++
	}
	if count > 1 {
		return output.NewError("usage.conflicting_targets",
			"restore: at most one of --to, --to-lsn, --to-name may be set").Wrap(output.ErrUsage)
	}

	cli, err := newDispatchClient(&opts.dispatch)
	if err != nil {
		return err
	}

	// Body shape mirrors handleEnqueueRestore in
	// internal/server/routes.go. Optional fields are only emitted
	// when set so an operator who doesn't pass --to doesn't end up
	// with a "to": "" field that the agent would have to ignore.
	body := map[string]any{
		"backup_id":       opts.backupID, // "latest" is resolved server-side
		"target_dir":      opts.targetDir,
		"allow_overwrite": opts.force,
	}
	if opts.repoURL != "" {
		body["repo"] = opts.repoURL
	}
	if opts.toLSN != "" {
		body["to_lsn"] = opts.toLSN
	}
	if opts.toTime != "" {
		body["to"] = opts.toTime
	}
	if opts.toName != "" {
		body["to_name"] = opts.toName
	}
	if opts.toAction != "" && opts.toAction != "pause" {
		// "pause" is the agent-side default; sending it explicitly is
		// noise.
		body["to_action"] = opts.toAction
	}
	if opts.toTimeline != "" && opts.toTimeline != "latest" {
		body["to_timeline"] = opts.toTimeline
	}
	if opts.toLatest {
		// Validated above against --to/--to-lsn/--to-name by
		// validateRestoreTargets; the agent re-checks.
		body["to_latest"] = true
	}
	if opts.skipGapCheck {
		body["skip_gap_check"] = true
	}
	if opts.forceForeign {
		body["allow_foreign_cluster"] = true
	}
	// --verify is forwarded only when the operator set it: the agent's
	// verify_after has always defaulted to "no verification" for
	// control-plane restores, and silently turning the local default
	// (auto) on for every dispatched restore would change what an
	// unchanged command line does.
	if cmd.Flags().Changed("verify") {
		if _, err := restore.ParseVerifyMode(opts.verifyMode); err != nil {
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("restore: --verify: %v", err)).Wrap(output.ErrUsage)
		}
		body["verify_after"] = opts.verifyMode
	}
	if opts.toExclusive {
		// CLI flag is `--to-exclusive` (default false → server-side
		// default true for to_inclusive). When the operator opts out
		// of inclusive recovery, send to_inclusive=false.
		body["to_inclusive"] = false
	}
	if len(opts.tablespaceMapping) > 0 {
		// Validate at the CLI side too so a typo'd entry
		// surfaces before the network round-trip. The agent
		// will re-validate; both layers gate.
		if _, err := restore.ParseTablespaceRemap(opts.tablespaceMapping); err != nil {
			return output.NewError("usage.bad_tablespace_mapping",
				fmt.Sprintf("restore: %v", err)).Wrap(output.ErrUsage)
		}
		body["tablespace_mapping"] = opts.tablespaceMapping
	}

	id, err := cli.EnqueueRestore(cmd.Context(), opts.deployment, body)
	if err != nil {
		return output.NewError("dispatch.enqueue_failed",
			fmt.Sprintf("restore: enqueue: %v", err)).Wrap(err)
	}

	// Suppress per-event forwarding when the operator picked the
	// json renderer — JSON mode is "one Result document per command",
	// not a stream of events. NDJSON / text get the live event flow.
	rendererName := d.Renderer().Name()
	suppressEvents := rendererName == "json"

	if !suppressEvents {
		_ = d.Event(cmd.Context(), output.NewEvent(output.SeverityInfo, "restore", "dispatch.enqueued").
			WithBody(map[string]any{
				"job_id":        id,
				"deployment":    opts.deployment,
				"control_plane": opts.dispatch.controlPlane,
			}))
	}

	// Forward progress events as they arrive. Each ProgressEvt
	// becomes a local dispatcher event so an NDJSON renderer sees a
	// stream of events identical to a local invocation. JSON mode
	// suppresses these (single-document contract).
	progressFn := func(ev ProgressEvt) {
		if suppressEvents {
			return
		}
		body := map[string]any{
			"at":   ev.At,
			"op":   ev.Op,
			"body": ev.Body,
		}
		_ = d.Event(cmd.Context(), output.NewEvent(output.SeverityInfo, "restore", "dispatch.progress").
			WithBody(body))
	}
	job, err := cli.PollUntilTerminal(cmd.Context(), id, progressFn)
	if err != nil {
		return dispatchPollErr(cmd.Context(), cli, "restore", id, err)
	}

	// Map the terminal state to a CLI exit. Failed/cancelled get
	// non-zero; completed gets the same Result envelope a local run
	// would produce.
	// Map the terminal state to a CLI exit. Failed/cancelled fall
	// through to the structured-error path; the code prefix
	// determines the exit code (restore.failed → ExitError).
	switch job.State {
	case "failed":
		return output.NewError("restore.failed",
			fmt.Sprintf("restore: job %s failed: %s", job.ID, job.Failure))
	case "cancelled":
		return output.NewError("aborted.restore_cancelled",
			fmt.Sprintf("restore: job %s cancelled: %s", job.ID, job.Failure))
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(restoreCPResultBody{
		JobID:      job.ID,
		Deployment: job.Deployment,
		AssignedTo: job.AssignedTo,
		Result:     job.Result,
	}))
}

// restoreCPResultBody is the shape we render when the control-plane
// path succeeds. The Result map is whatever the agent's RestoreExecutor
// returned (backup_id, file_count, bytes_written, ...).
type restoreCPResultBody struct {
	JobID      string         `json:"job_id"`
	Deployment string         `json:"deployment"`
	AssignedTo string         `json:"assigned_to,omitempty"`
	Result     map[string]any `json:"result,omitempty"`
}

// WriteText renders the human-readable summary.
func (b restoreCPResultBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ restore dispatched and completed\n")
	fmt.Fprintf(bw, "  Job:        %s\n", b.JobID)
	fmt.Fprintf(bw, "  Deployment: %s\n", b.Deployment)
	if b.AssignedTo != "" {
		fmt.Fprintf(bw, "  Agent:      %s\n", b.AssignedTo)
	}
	if id, ok := b.Result["backup_id"].(string); ok && id != "" {
		fmt.Fprintf(bw, "  Backup:     %s\n", id)
	}
	if td, ok := b.Result["target_dir"].(string); ok && td != "" {
		fmt.Fprintf(bw, "  Target:     %s\n", td)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
