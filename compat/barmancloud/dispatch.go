// dispatch.go — barman-cloud shim dispatcher: invokes internal/cli with captured stdout/stderr for auto-init detection.
package barmancloud

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// dispatchResult mirrors compat/walg's structure; we don't
// import that package because internal/cli isn't its
// dependency surface and we want barmancloud to compile
// independently of the walg package's evolution.
type dispatchResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// dispatchNative runs the native CLI with cobra's stdout +
// stderr captured into byte buffers.  Returns the exit code
// + buffers so the caller can inspect for `notfound.repo`
// (auto-init trigger) before deciding whether to forward
// the captured output to the operator's view.
//
// var-typed so tests can substitute a stub.
var dispatchNative = func(args []string) dispatchResult {
	root := cli.NewRoot()
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	rc := cli.Run(root)
	return dispatchResult{
		ExitCode: rc,
		Stdout:   outBuf.Bytes(),
		Stderr:   errBuf.Bytes(),
	}
}

// dispatchWithAutoInit runs a push-shaped native command
// (wal push, backup, etc.) and, on `notfound.repo`,
// transparently runs `pg_hardstorage repo init <url>` then
// retries the push once.  Same shape as the walg shim's
// helper, duplicated to keep barmancloud's compile graph
// independent.
//
// auditWriter receives a one-line breadcrumb when auto-init
// fires.  Set to nil to suppress.
func dispatchWithAutoInit(auditWriter io.Writer, repoURL string, pushArgs []string) int {
	res := dispatchNative(pushArgs)
	if res.ExitCode == 0 || !looksLikeMissingRepo(res) {
		forwardCaptured(res)
		return res.ExitCode
	}

	if auditWriter != nil {
		fmt.Fprintf(auditWriter,
			"pg-hardstorage-barmancloud: repo not initialised at %s; auto-initialising before push\n",
			repoURL)
	}
	initRes := dispatchNative([]string{"repo", "init", repoURL})
	switch {
	case initRes.ExitCode == 0:
		// fresh init — proceed.
	case looksLikeRepoExists(initRes):
		// race: another invocation initialised concurrently.
	default:
		// init failed for a non-recoverable reason — surface
		// that error rather than the original notfound.repo.
		forwardCaptured(initRes)
		return initRes.ExitCode
	}

	res = dispatchNative(pushArgs)
	forwardCaptured(res)
	return res.ExitCode
}

// forwardCaptured replays a captured dispatch's stdout +
// stderr to the real process file descriptors.  The captured
// content reaches the operator's view; the shim's audit
// breadcrumb stays separate.
func forwardCaptured(res dispatchResult) {
	if len(res.Stdout) > 0 {
		_, _ = os.Stdout.Write(res.Stdout)
	}
	if len(res.Stderr) > 0 {
		_, _ = os.Stderr.Write(res.Stderr)
	}
}

func looksLikeMissingRepo(res dispatchResult) bool {
	if res.ExitCode != int(output.ExitNotFound) {
		return false
	}
	return containsCode(res.Stdout, "notfound.repo") ||
		containsCode(res.Stderr, "notfound.repo")
}

func looksLikeRepoExists(res dispatchResult) bool {
	if res.ExitCode != int(output.ExitConflict) {
		return false
	}
	return containsCode(res.Stdout, "conflict.repo_exists") ||
		containsCode(res.Stderr, "conflict.repo_exists")
}

func containsCode(buf []byte, code string) bool {
	return bytes.Contains(buf, []byte(`"code": "`+code+`"`)) ||
		bytes.Contains(buf, []byte(`"code":"`+code+`"`))
}
