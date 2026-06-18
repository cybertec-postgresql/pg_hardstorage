// verify_controlplane.go — '--control-plane' mode of `verify`: POST + poll a remote-agent verify job.
package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// runVerifyControlPlane is the --control-plane mode of `pg_hardstorage
// verify`. POSTs the request to /v1/deployments/<n>/verifies and polls
// /v1/jobs/<id> until terminal. The agent's VerifyExecutor performs
// the full restore-to-sandbox + pg_verifybackup loop on its own host;
// the operator's machine doesn't need Docker.
//
// Always implies --full semantics — fast verify is a local-only
// concern (it just walks chunks via the CAS, no network round-trip
// would help).
func runVerifyControlPlane(cmd *cobra.Command, deployment, backupID, repoURL, pgMajor string, dispatch *dispatchAuthFlags) error {
	d := DispatcherFrom(cmd)

	cli, err := newDispatchClient(dispatch)
	if err != nil {
		return err
	}

	// Body shape mirrors handleEnqueueVerify in
	// internal/server/routes.go. backup_id is required; pg_major and
	// repo are optional (server / agent fill in defaults).
	body := map[string]any{
		"backup_id": backupID,
	}
	if repoURL != "" {
		body["repo"] = repoURL
	}
	if pgMajor != "" {
		body["pg_major"] = pgMajor
	}

	id, err := cli.EnqueueVerify(cmd.Context(), deployment, body)
	if err != nil {
		return output.NewError("dispatch.enqueue_failed",
			fmt.Sprintf("verify: enqueue: %v", err)).Wrap(err)
	}

	rendererName := d.Renderer().Name()
	suppressEvents := rendererName == "json"

	if !suppressEvents {
		_ = d.Event(cmd.Context(), output.NewEvent(output.SeverityInfo, "verify", "dispatch.enqueued").
			WithBody(map[string]any{
				"job_id":        id,
				"deployment":    deployment,
				"control_plane": dispatch.controlPlane,
			}))
	}

	progressFn := func(ev ProgressEvt) {
		if suppressEvents {
			return
		}
		_ = d.Event(cmd.Context(), output.NewEvent(output.SeverityInfo, "verify", "dispatch.progress").
			WithBody(map[string]any{
				"at":   ev.At,
				"op":   ev.Op,
				"body": ev.Body,
			}))
	}
	job, err := cli.PollUntilTerminal(cmd.Context(), id, progressFn)
	if err != nil {
		return output.NewError("dispatch.poll_failed",
			fmt.Sprintf("verify: poll: %v", err)).Wrap(err)
	}

	switch job.State {
	case "failed":
		// pg_verifybackup found a discrepancy. The agent's
		// VerifyExecutor already wrote the structured tool output
		// into the Result before failing the job, so we surface it
		// here for triage.
		return output.NewError("verify.failed",
			fmt.Sprintf("verify: job %s failed: %s", job.ID, job.Failure)).
			WithSuggestion(&output.Suggestion{
				Human: "the bytes are intact (chunk SHA-256 round-trip would have caught corruption); pg_verifybackup found a structural discrepancy. Capture the agent's job result via `pg_hardstorage --control-plane <url> get-job " + job.ID + "` for the full tool output.",
			})
	case "cancelled":
		return output.NewError("aborted.verify_cancelled",
			fmt.Sprintf("verify: job %s cancelled: %s", job.ID, job.Failure))
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(verifyCPResultBody{
		JobID:      job.ID,
		Deployment: job.Deployment,
		AssignedTo: job.AssignedTo,
		Result:     job.Result,
	}))
}

// verifyCPResultBody renders the terminal Result for the control-
// plane path. The agent's VerifyExecutor returns the sandbox.Result
// fields (passed, pg_major, image, tool_stdout, etc.).
type verifyCPResultBody struct {
	JobID      string         `json:"job_id"`
	Deployment string         `json:"deployment"`
	AssignedTo string         `json:"assigned_to,omitempty"`
	Result     map[string]any `json:"result,omitempty"`
}

// WriteText renders the human-readable summary.
func (b verifyCPResultBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ verify dispatched and completed\n")
	fmt.Fprintf(bw, "  Job:        %s\n", b.JobID)
	fmt.Fprintf(bw, "  Deployment: %s\n", b.Deployment)
	if b.AssignedTo != "" {
		fmt.Fprintf(bw, "  Agent:      %s\n", b.AssignedTo)
	}
	if id, ok := b.Result["backup_id"].(string); ok && id != "" {
		fmt.Fprintf(bw, "  Backup:     %s\n", id)
	}
	if pg, ok := b.Result["pg_major"].(string); ok && pg != "" {
		fmt.Fprintf(bw, "  PG major:   %s\n", pg)
	}
	if passed, ok := b.Result["passed"].(bool); ok {
		if passed {
			fmt.Fprintf(bw, "  Result:     ✓ pg_verifybackup passed\n")
		} else if skipped, _ := b.Result["skipped"].(bool); skipped {
			reason, _ := b.Result["skip_reason"].(string)
			fmt.Fprintf(bw, "  Result:     ⊘ skipped (%s)\n", reason)
		} else {
			fmt.Fprintf(bw, "  Result:     ✗ pg_verifybackup FAILED\n")
		}
	}
	if v, ok := b.Result["duration_ms"].(float64); ok {
		fmt.Fprintf(bw, "  Duration:   %.0f ms\n", v)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// silence-unused: cobra is referenced for the parameter type only.
var _ = (*cobra.Command)(nil)
