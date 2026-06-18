package cli

import (
	"errors"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestErrorCode_StructuredOutputError extracts the typed code.
func TestErrorCode_StructuredOutputError(t *testing.T) {
	err := output.NewError("restore.target_in_wal_gap", "boom")
	if got := errorCode(err); got != "restore.target_in_wal_gap" {
		t.Errorf("got %q, want restore.target_in_wal_gap", got)
	}
}

// TestErrorCode_BareError returns "".
func TestErrorCode_BareError(t *testing.T) {
	if got := errorCode(errors.New("plain")); got != "" {
		t.Errorf("got %q, want empty (no structured code on a bare error)", got)
	}
}

func TestErrorCode_Nil(t *testing.T) {
	if got := errorCode(nil); got != "" {
		t.Errorf("got %q, want empty for nil error", got)
	}
}

// TestMatchAutoOnError_RestoreTargetInWalGap: the builtin
// 'restore' skill declares auto_on_error including
// `restore.target_in_wal_gap`.  Verifying that lookup fires.
func TestMatchAutoOnError_RestoreTargetInWalGap(t *testing.T) {
	got := matchAutoOnError("restore.target_in_wal_gap")
	if got == nil {
		t.Fatal("expected the restore skill to match")
	}
	if got.Name != "restore" {
		t.Errorf("matched skill = %q, want restore", got.Name)
	}
}

func TestMatchAutoOnError_UnknownReturnsNil(t *testing.T) {
	if got := matchAutoOnError("not.a.real.code"); got != nil {
		t.Errorf("expected nil for unknown code; got %+v", got)
	}
}

func TestMatchAutoOnError_EmptyReturnsNil(t *testing.T) {
	if got := matchAutoOnError(""); got != nil {
		t.Errorf("expected nil for empty code")
	}
}

// TestAutoLaunchEnabled_FlagWins: setting --on-error-llm on the
// root command flips the gate.
func TestAutoLaunchEnabled_FlagWins(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_ON_ERROR_LLM", "")
	root := &cobra.Command{}
	root.PersistentFlags().Bool("on-error-llm", false, "")
	if autoLaunchEnabled(root) {
		t.Error("default false should be off")
	}
	_ = root.PersistentFlags().Set("on-error-llm", "true")
	if !autoLaunchEnabled(root) {
		t.Error("--on-error-llm=true should enable")
	}
}

// TestAutoLaunchEnabled_EnvFallback: the env var alone is enough.
func TestAutoLaunchEnabled_EnvFallback(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_ON_ERROR_LLM", "1")
	root := &cobra.Command{}
	if !autoLaunchEnabled(root) {
		t.Error("env=1 should enable")
	}
	t.Setenv("PG_HARDSTORAGE_ON_ERROR_LLM", "yes")
	if !autoLaunchEnabled(root) {
		t.Error("env=yes should enable")
	}
	t.Setenv("PG_HARDSTORAGE_ON_ERROR_LLM", "0")
	if autoLaunchEnabled(root) {
		t.Error("env=0 should NOT enable")
	}
}

// TestHasLLMAncestor: direct self-match + parent chain match.
func TestHasLLMAncestor(t *testing.T) {
	root := &cobra.Command{Use: "pg_hardstorage"}
	llm := &cobra.Command{Use: "llm"}
	chat := &cobra.Command{Use: "chat"}
	root.AddCommand(llm)
	llm.AddCommand(chat)

	if !hasLLMAncestor(llm) {
		t.Error("llm itself should match")
	}
	if !hasLLMAncestor(chat) {
		t.Error("chat under llm should match")
	}
	other := &cobra.Command{Use: "doctor"}
	root.AddCommand(other)
	if hasLLMAncestor(other) {
		t.Error("doctor should not match")
	}
}

// TestShouldAutoLaunchLLM_Disabled: when neither flag nor env is
// set, the gate is closed regardless of error shape.
func TestShouldAutoLaunchLLM_Disabled(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_ON_ERROR_LLM", "")
	root := &cobra.Command{}
	root.PersistentFlags().Bool("on-error-llm", false, "")
	err := output.NewError("restore.target_in_wal_gap", "x")
	if shouldAutoLaunchLLM(root, err) {
		t.Error("disabled gate should refuse auto-launch")
	}
}
