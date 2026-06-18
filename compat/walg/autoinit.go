// autoinit.go — WAL-G shim transparent `repo init` retry on first push when the repo doesn't yet exist.
package walg

import (
	"bytes"
	"fmt"
	"io"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// dispatchWithAutoInit runs a native push command (wal push /
// backup full) and, on `notfound.repo`, transparently runs
// `pg_hardstorage repo init <url>` then retries the push once.
//
// Why: real wal-g and pgBackRest auto-create their bucket
// structure on first archive — operators don't run a separate
// `init` step.  Our native CLI requires `repo init` first; if
// the K8s drop-in scenario is to look identical to those tools
// from the operator's perspective, the shim has to bridge that
// gap.
//
// Bounded to ONE retry: if the post-init push still fails,
// surface the error rather than retrying indefinitely.  Any
// failure other than notfound.repo (auth, network, real
// missing object, ...) is forwarded to the caller without an
// init attempt.
//
// auditWriter receives a one-line breadcrumb whenever auto-init
// fires, so the operator sees in their own logs that the shim
// performed the init transparently.
func dispatchWithAutoInit(auditWriter io.Writer, repoURL string, pushArgs []string) int {
	res := dispatchNativeCapture(pushArgs)
	if res.ExitCode == 0 || !looksLikeMissingRepo(res) {
		// Common case: success, OR a different failure we
		// shouldn't try to "fix" with init.  Forward and return.
		forwardCaptured(res)
		return res.ExitCode
	}

	// Auto-init.  We DON'T forward the original error output
	// to the operator's stdout/stderr — the auto-init succeeds
	// (or fails loudly) and the operator's view is the
	// post-init state, not the transient pre-init failure.
	if auditWriter != nil {
		fmt.Fprintf(auditWriter,
			"pg-hardstorage-walg: repo not initialised at %s; auto-initialising before push\n",
			repoURL)
	}
	initRes := dispatchNativeCapture([]string{"repo", "init", repoURL})
	switch {
	case initRes.ExitCode == 0:
		// fresh init — proceed.
	case looksLikeRepoExists(initRes):
		// Race: another shim invocation init'd it between our
		// first push and this init.  Treat as success and
		// retry the push.
		if auditWriter != nil {
			fmt.Fprintln(auditWriter,
				"pg-hardstorage-walg: repo init reported already-exists; another invocation initialised concurrently")
		}
	default:
		// init failed for a non-recoverable reason (auth, bad
		// URL, missing keyring).  Surface the init failure
		// — that's the actual root cause.
		forwardCaptured(initRes)
		return initRes.ExitCode
	}

	// Retry the push.  Single retry; a second notfound.repo
	// would mean the init didn't actually persist (e.g. the
	// bucket is genuinely unwritable) and the operator should
	// see the failure straight.
	res = dispatchNativeCapture(pushArgs)
	forwardCaptured(res)
	return res.ExitCode
}

// looksLikeMissingRepo returns true when the captured dispatch
// output contains the `notfound.repo` error code.  We match on
// the structured-error JSON's `"code":"notfound.repo"` rather
// than the human-readable message — the former is the v1 stable
// contract per docs/SPEC.md, the latter changes between releases.
//
// We scan BOTH stdout AND stderr because the native CLI's error
// renderer routes structured errors to stderr (per the
// internal/output convention) — the earlier "stdout-only" match
// silently no-op'd in production.
//
// Belt-and-suspenders also requires the exit code is
// ExitNotFound (6) — covers the case where another tool's
// output happens to contain the literal string.
func looksLikeMissingRepo(res dispatchResult) bool {
	if res.ExitCode != int(output.ExitNotFound) {
		return false
	}
	return containsCode(res.Stdout, "notfound.repo") ||
		containsCode(res.Stderr, "notfound.repo")
}

// looksLikeRepoExists returns true when `repo init` failed
// because the repo is already present.  Used to swallow the
// idempotent-already-exists case during auto-init's race window.
func looksLikeRepoExists(res dispatchResult) bool {
	if res.ExitCode != int(output.ExitConflict) {
		return false
	}
	return containsCode(res.Stdout, "conflict.repo_exists") ||
		containsCode(res.Stderr, "conflict.repo_exists")
}

// containsCode is the substring test that handles both
// pretty-printed (`"code": "X"`, with space) and compact
// (`"code":"X"`, no space) JSON.
func containsCode(buf []byte, code string) bool {
	return bytes.Contains(buf, []byte(`"code": "`+code+`"`)) ||
		bytes.Contains(buf, []byte(`"code":"`+code+`"`))
}
