// llm_autolaunch.go — --on-error-llm gate that drops the operator into a matching LLM helper skill.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/skills"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// shouldAutoLaunchLLM reports whether the failure that just
// stopped the command should drop the operator into a matching
// LLM helper skill.
//
// Conditions (all must hold):
//
//   - --on-error-llm flag is set, OR PG_HARDSTORAGE_ON_ERROR_LLM=1.
//   - The error carries a structured Code (not a bare error
//     string).
//   - A loaded skill declares `auto_on_error: [<code>]` matching
//     the failure code.
//   - We're attached to a TTY — auto-launch in CI / pipes is
//     useless and noisy.
//   - The failing command isn't itself the LLM helper (don't
//     loop into ourselves on a chat-session error).
func shouldAutoLaunchLLM(cmd *cobra.Command, err error) bool {
	if !autoLaunchEnabled(cmd) {
		return false
	}
	if !stderrIsTTY(cmd) {
		return false
	}
	if cmd.Name() == "llm" || hasLLMAncestor(cmd) {
		return false
	}
	code := errorCode(err)
	if code == "" {
		return false
	}
	skill := matchAutoOnError(code)
	return skill != nil
}

// launchAutoLLM spawns the matching skill in-process.  The
// failure context (command path + structured error code +
// message) is passed as the first user message so the
// assistant arrives already grounded in what just failed.
func launchAutoLLM(cmd *cobra.Command, runErr error) error {
	code := errorCode(runErr)
	skill := matchAutoOnError(code)
	if skill == nil {
		return errors.New("auto-launch: no skill matches code " + code)
	}

	// Banner so the operator understands why a chat suddenly
	// appeared instead of the bare error.
	banner := fmt.Sprintf(
		"\n[auto-launch] %s failed with code %q\n"+
			"[auto-launch] dropping into the %q skill (--on-error-llm)\n"+
			"[auto-launch] type /exit to return to the shell\n\n",
		cmd.CommandPath(), code, skill.Name)
	fmt.Fprint(cmd.ErrOrStderr(), banner)

	// First user message: a one-liner describing the failure.
	// The assistant uses this as context for its first response;
	// the operator can elaborate from there.
	firstMessage := fmt.Sprintf("My last command (%s) just failed with error code %q. The error message was: %v",
		cmd.CommandPath(), code, runErr)

	return runLlmChat(cmd, llmChatOptions{
		skill:              skill.Name,
		initialUserMessage: firstMessage,
		// We don't override provider/endpoint/model — auto-launch
		// uses whatever the operator's standing config says.
	})
}

// autoLaunchEnabled reports whether the auto-launch flag is set
// (either via --on-error-llm or PG_HARDSTORAGE_ON_ERROR_LLM=1).
func autoLaunchEnabled(cmd *cobra.Command) bool {
	if cmd != nil {
		if v, _ := cmd.Flags().GetBool("on-error-llm"); v {
			return true
		}
		// Also walk up parents in case the flag is set on the
		// root and cobra hasn't propagated it.
		if root := cmd.Root(); root != nil {
			if v, _ := root.PersistentFlags().GetBool("on-error-llm"); v {
				return true
			}
		}
	}
	switch strings.ToLower(os.Getenv("PG_HARDSTORAGE_ON_ERROR_LLM")) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// stderrIsTTY reports whether stderr looks like a real terminal.
// Conservative: we only return true when we can stat the FD and
// it's a character device.  In the bytes.Buffer case (tests) we
// return false, suppressing auto-launch.
func stderrIsTTY(cmd *cobra.Command) bool {
	w := cmd.ErrOrStderr()
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// hasLLMAncestor reports whether any ancestor of cmd is the
// `llm` command — used to suppress auto-launch when an LLM
// subcommand itself failed (avoid recursion).
func hasLLMAncestor(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Name() == "llm" {
			return true
		}
	}
	return false
}

// errorCode extracts the structured error code from err.
// Returns "" when the error doesn't carry one.
func errorCode(err error) string {
	if err == nil {
		return ""
	}
	var oerr *output.Error
	if errors.As(err, &oerr) {
		return oerr.Code
	}
	return ""
}

// matchAutoOnError walks the loaded skill set and returns the
// first skill whose `auto_on_error` list contains code.
// Returns nil when no skill matches.
//
// Loading failures degrade silently — the auto-launch path is
// best-effort by design.
func matchAutoOnError(code string) *skills.Skill {
	if code == "" {
		return nil
	}
	set, err := loadSkillSet()
	if err != nil {
		return nil
	}
	for _, sk := range set.All() {
		for _, want := range sk.Trigger.AutoOnError {
			if want == code {
				return sk
			}
		}
	}
	return nil
}

// silence unused-import warnings.
var _ io.Writer
