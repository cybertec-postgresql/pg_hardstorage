// verify.go — VerifyMode (auto/skip/require): post-restore pg_verifybackup gate.
package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// VerifyMode controls whether and how Verify runs as a post-restore gate.
type VerifyMode string

const (
	// VerifyAuto runs pg_verifybackup if it's on PATH; if absent, the
	// restore succeeds with a warning that verification was skipped.
	VerifyAuto VerifyMode = "auto"
	// VerifySkip never runs verification. Useful for fast iteration
	// when the operator already trusts the bytes (e.g. test setups).
	VerifySkip VerifyMode = "skip"
	// VerifyRequire treats verification as a hard gate: a missing
	// pg_verifybackup binary OR a non-zero exit fails the restore.
	VerifyRequire VerifyMode = "require"
)

// ParseVerifyMode is the inverse of the constants above. Unknown
// values return a usage-shaped error so the CLI can surface typos.
func ParseVerifyMode(s string) (VerifyMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return VerifyAuto, nil
	case "skip", "off", "no", "none":
		return VerifySkip, nil
	case "require", "required", "yes":
		return VerifyRequire, nil
	}
	return "", output.NewError("usage.bad_verify_mode",
		fmt.Sprintf("verify: unknown mode %q (auto|skip|require)", s)).Wrap(output.ErrUsage)
}

// VerifyResult is the structured outcome of a verification attempt.
//
// Status covers four cases the CLI cares about:
//
//	"skipped"         — VerifySkip was set, or VerifyAuto saw no binary
//	"missing_tool"    — VerifyAuto noted the absent binary; restore proceeds
//	"passed"          — pg_verifybackup exited 0
//	"failed"          — pg_verifybackup exited non-zero
//
// Duration serializes as WHOLE MILLISECONDS under the frozen key
// duration_ms (MarshalJSON below) — a raw time.Duration would emit
// nanoseconds under a _ms key.
type VerifyResult struct {
	Mode     VerifyMode    `json:"mode"`
	Status   string        `json:"status"`
	ToolPath string        `json:"tool_path,omitempty"`
	ExitCode int           `json:"exit_code,omitempty"`
	Stdout   string        `json:"stdout,omitempty"`
	Stderr   string        `json:"stderr,omitempty"`
	Duration time.Duration `json:"-"`
}

// MarshalJSON emits duration_ms as whole milliseconds.
func (v VerifyResult) MarshalJSON() ([]byte, error) {
	type alias VerifyResult
	return json.Marshal(struct {
		alias
		DurationMS int64 `json:"duration_ms,omitempty"`
	}{alias(v), v.Duration.Milliseconds()})
}

// UnmarshalJSON is the inverse of MarshalJSON (ms → time.Duration).
func (v *VerifyResult) UnmarshalJSON(b []byte) error {
	type alias VerifyResult
	aux := struct {
		*alias
		DurationMS int64 `json:"duration_ms,omitempty"`
	}{alias: (*alias)(v)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	v.Duration = time.Duration(aux.DurationMS) * time.Millisecond
	return nil
}

// Verify runs the post-restore verification gate against target.
//
// Behaviour by mode:
//
//	auto     - run if pg_verifybackup is on PATH; result.Status =
//	           "missing_tool" if absent (no error returned).
//	skip     - return result.Status = "skipped" without trying.
//	require  - run; "missing_tool" or non-zero exit returns an error
//	           classified as verify.failed (exit code 9).
//
// Verify never returns an error for the auto-skipped-tool case. The
// caller decides what to do with VerifyResult.Status.
func Verify(ctx context.Context, target string, mode VerifyMode) (*VerifyResult, error) {
	res := &VerifyResult{Mode: mode}
	switch mode {
	case VerifySkip:
		res.Status = "skipped"
		return res, nil
	case VerifyAuto, VerifyRequire:
		// fall through
	default:
		return nil, output.NewError("usage.bad_verify_mode",
			fmt.Sprintf("verify: unknown mode %q", mode)).Wrap(output.ErrUsage)
	}

	path, err := exec.LookPath("pg_verifybackup")
	if err != nil {
		res.Status = "missing_tool"
		if mode == VerifyRequire {
			return res, output.NewError("verify.missing_tool",
				"pg_verifybackup not found on PATH (--verify=require)").
				WithSuggestion(&output.Suggestion{
					Human: "install postgresql-client or set --verify=skip",
				})
		}
		return res, nil
	}
	res.ToolPath = path

	start := time.Now()
	cmd := exec.CommandContext(ctx, path, target)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	res.Duration = time.Since(start)
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()

	if runErr == nil {
		res.Status = "passed"
		res.ExitCode = 0
		return res, nil
	}

	// Non-zero exit. Capture the exit code if we can.
	res.Status = "failed"
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
	} else {
		res.ExitCode = -1
	}
	if mode == VerifyRequire {
		return res, output.NewError("verify.checksum_mismatch",
			fmt.Sprintf("pg_verifybackup failed (exit %d): %s",
				res.ExitCode, summarize(res.Stderr))).
			WithSuggestion(&output.Suggestion{
				Human: "inspect target with `pg_verifybackup` directly; consider re-running restore",
			})
	}
	// Auto mode: surface the failure as a Result field but don't
	// return an error — caller logs it and continues.
	return res, nil
}

// summarize returns a short single-line head of body, useful for
// error messages. We don't dump multi-KB stderr into a typed error.
func summarize(body string) string {
	body = strings.TrimSpace(body)
	if i := strings.IndexAny(body, "\n\r"); i >= 0 {
		body = body[:i]
	}
	if len(body) > 200 {
		body = body[:200] + "..."
	}
	return body
}
