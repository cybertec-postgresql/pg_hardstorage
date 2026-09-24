// backup_controlplane.go — '--control-plane' mode of `backup`: POST + poll a remote-agent job.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

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

	// Flags the agent cannot honour are refused, not dropped: each one
	// changes what backup gets taken (or how safely), so a dispatch
	// that ignored it would report success for a backup the operator
	// did not ask for. Checked before anything is sent.
	if err := refuseUnsupportedControlPlaneFlags(cmd, "backup", map[string]string{
		"pg-connection":          "the agent connects with its own deployment config",
		"kek":                    "the KEK comes from the agent's deployment kek_ref / keyring",
		"kms-config":             "cloud-KMS settings come from the agent's own config (kms.providers)",
		"tde":                    "source TDE is declared in the agent's deployment tde: block",
		"tde-engine":             "source TDE is declared in the agent's deployment tde: block",
		"tde-key-ref":            "source TDE is declared in the agent's deployment tde: block",
		"allow-concurrent":       "the backup lease is governed by the agent's deployment config",
		"ignore-capacity":        "the agent runs no capacity pre-flight to skip",
		"capacity-safety-factor": "the agent runs no capacity pre-flight",
		"verbose":                "per-file progress is not streamed through the control plane",
	}); err != nil {
		return err
	}

	// Body fields ride into Job.Args. The agent's BackupExecutor
	// reads:
	//   - fast (bool)
	//   - label (string)
	//   - include_wal (bool)
	//   - incremental_from (backup ID or "latest")
	//   - stall_timeout (duration string; --stall-timeout)
	//   - inactivity_timeout (duration string; API-only streaming
	//     watchdog override, no CLI flag)
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
	if opts.includeWAL {
		body["include_wal"] = true
	}
	if opts.incrementalFrom != "" {
		body["incremental_from"] = opts.incrementalFrom
	}
	if opts.stallTimeout > 0 {
		body["stall_timeout"] = opts.stallTimeout.String()
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
		return dispatchPollErr(cmd.Context(), cli, "backup", id, err)
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

// refuseUnsupportedControlPlaneFlags returns a usage.unsupported_flag
// error for the first flag in unsupported the operator explicitly set
// on cmd. Keyed on Changed (not the value) so a flag whose default is
// non-empty -- --verify-restore=auto -- only trips when the operator
// actually typed it. The map value says why the agent cannot honour
// the flag. Iterated in sorted order so the refusal is deterministic.
func refuseUnsupportedControlPlaneFlags(cmd *cobra.Command, verb string, unsupported map[string]string) error {
	names := make([]string, 0, len(unsupported))
	for n := range unsupported {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if f := cmd.Flags().Lookup(n); f != nil && f.Changed {
			return output.NewError("usage.unsupported_flag",
				fmt.Sprintf("%s --control-plane: --%s is not supported in control-plane mode (%s); remove the flag or run the %s locally",
					verb, n, unsupported[n], verb)).
				Wrap(output.ErrUsage)
		}
	}
	return nil
}

// dispatchPollErr maps a PollUntilTerminal failure to the CLI error.
// An interrupted poll (Ctrl-C / SIGTERM cancels the command context)
// is NOT a poll failure: the operator stopped waiting, so the job is
// cancelled on the control plane -- leaving a restore or backup running
// unattended after the operator hit Ctrl-C is the surprising outcome --
// and the command exits with the aborted code (5). The cancel request
// uses a fresh, short context because ctx is already done.
func dispatchPollErr(ctx context.Context, cli *DispatchClient, verb, jobID string, err error) error {
	if ctx.Err() == nil || !errors.Is(err, ctx.Err()) {
		return output.NewError("dispatch.poll_failed",
			fmt.Sprintf("%s: poll: %v", verb, err)).Wrap(err)
	}
	cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msg := fmt.Sprintf("%s: interrupted; cancelled control-plane job %s", verb, jobID)
	if cerr := cancelDispatchedJob(cctx, cli, jobID, "operator interrupted the dispatching CLI"); cerr != nil {
		msg = fmt.Sprintf("%s: interrupted; could NOT cancel control-plane job %s, which may still be running (cancel it with POST /v1/jobs/%s/cancel): %v",
			verb, jobID, jobID, cerr)
	}
	return output.NewError("aborted.context_cancelled", msg).Wrap(err)
}

// cancelDispatchedJob POSTs /v1/jobs/<id>/cancel.
func cancelDispatchedJob(ctx context.Context, cli *DispatchClient, jobID, reason string) error {
	if err := cli.ensureClient(); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		cli.BaseURL+"/v1/jobs/"+url.PathEscape(jobID)+"/cancel", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	cli.applyAuth(req)
	resp, err := cli.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}
