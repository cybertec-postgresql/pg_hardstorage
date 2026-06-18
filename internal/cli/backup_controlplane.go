// backup_controlplane.go — '--control-plane' mode of `backup`: POST + poll a remote-agent job.
package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// runBackupControlPlane is the --control-plane mode of `pg_hardstorage
// backup`. POSTs the request to the control plane's
// /v1/deployments/<n>/backups endpoint and polls /v1/jobs/<id> until
// the job reaches a terminal state.
//
// Operators reach for this when they want to take a backup of a
// remote PG without having a local pg_hardstorage installed alongside
// PG: an agent in the right network zone (sidecar, VM-local, etc.)
// claims and runs the job; the operator's CLI just orchestrates from
// wherever they are.
func runBackupControlPlane(cmd *cobra.Command, opts runOptions) error {
	d := DispatcherFrom(cmd)

	cli, err := newDispatchClient(&opts.dispatch)
	if err != nil {
		return err
	}

	// Body fields ride into Job.Args. The agent's BackupExecutor
	// reads:
	//   - fast (bool)
	//   - label (string)
	//   - inactivity_timeout (duration string)
	//
	// repo flows alongside Args so the server can fall back to its
	// own --repo when the operator doesn't pass one.
	body := map[string]any{}
	if opts.fast {
		body["fast"] = true
	}
	if opts.label != "" {
		body["label"] = opts.label
	}
	if opts.repoURL != "" {
		body["repo"] = opts.repoURL
	}
	// tenant + encrypt/no-encrypt are deployment-config concerns on
	// the agent side (the agent's local config picks the tenant; the
	// keyring picks the encryption posture). Passing them through the
	// API is a+ enhancement once we extend EnqueueOptions; for
	// now we surface a clear refusal so the operator isn't surprised
	// when their flag is silently ignored.
	if opts.tenant != "" && opts.tenant != "default" {
		return output.NewError("usage.unsupported_flag",
			"backup --control-plane: --tenant is set by the agent's local config, not the CLI; remove the flag or take the backup locally").
			Wrap(output.ErrUsage)
	}
	if opts.encrypt || opts.noEncrypt {
		return output.NewError("usage.unsupported_flag",
			"backup --control-plane: encryption posture is set by the agent's keyring, not --encrypt/--no-encrypt; remove the flag or take the backup locally").
			Wrap(output.ErrUsage)
	}

	id, err := cli.EnqueueBackup(cmd.Context(), opts.deployment, body)
	if err != nil {
		return output.NewError("dispatch.enqueue_failed",
			fmt.Sprintf("backup: enqueue: %v", err)).Wrap(err)
	}

	rendererName := d.Renderer().Name()
	suppressEvents := rendererName == "json"

	if !suppressEvents {
		_ = d.Event(cmd.Context(), output.NewEvent(output.SeverityInfo, "backup", "dispatch.enqueued").
			WithBody(map[string]any{
				"job_id":        id,
				"deployment":    opts.deployment,
				"control_plane": opts.dispatch.controlPlane,
			}))
	}

	progressFn := func(ev ProgressEvt) {
		if suppressEvents {
			return
		}
		_ = d.Event(cmd.Context(), output.NewEvent(output.SeverityInfo, "backup", "dispatch.progress").
			WithBody(map[string]any{
				"at":   ev.At,
				"op":   ev.Op,
				"body": ev.Body,
			}))
	}
	job, err := cli.PollUntilTerminal(cmd.Context(), id, progressFn)
	if err != nil {
		return output.NewError("dispatch.poll_failed",
			fmt.Sprintf("backup: poll: %v", err)).Wrap(err)
	}

	switch job.State {
	case "failed":
		return output.NewError("backup.failed",
			fmt.Sprintf("backup: job %s failed: %s", job.ID, job.Failure))
	case "cancelled":
		return output.NewError("aborted.backup_cancelled",
			fmt.Sprintf("backup: job %s cancelled: %s", job.ID, job.Failure))
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(backupCPResultBody{
		JobID:      job.ID,
		Deployment: job.Deployment,
		AssignedTo: job.AssignedTo,
		Result:     job.Result,
	}))
}

// backupCPResultBody renders the terminal Result for the control-
// plane path. The agent's BackupExecutor returns backup_id,
// duration, file_count, unique_chunk_count, etc. — we surface them
// in the CLI's normal Result envelope.
type backupCPResultBody struct {
	JobID      string         `json:"job_id"`
	Deployment string         `json:"deployment"`
	AssignedTo string         `json:"assigned_to,omitempty"`
	Result     map[string]any `json:"result,omitempty"`
}

// WriteText is the human-readable render.
func (b backupCPResultBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ backup dispatched and completed\n")
	fmt.Fprintf(bw, "  Job:        %s\n", b.JobID)
	fmt.Fprintf(bw, "  Deployment: %s\n", b.Deployment)
	if b.AssignedTo != "" {
		fmt.Fprintf(bw, "  Agent:      %s\n", b.AssignedTo)
	}
	if id, ok := b.Result["backup_id"].(string); ok && id != "" {
		fmt.Fprintf(bw, "  Backup:     %s\n", id)
	}
	if v, ok := b.Result["unique_chunk_count"].(float64); ok {
		fmt.Fprintf(bw, "  Chunks:     %.0f\n", v)
	}
	if v, ok := b.Result["logical_bytes"].(float64); ok {
		fmt.Fprintf(bw, "  Bytes:      %.0f\n", v)
	}
	if v, ok := b.Result["duration_ms"].(float64); ok {
		fmt.Fprintf(bw, "  Duration:   %.0f ms\n", v)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
