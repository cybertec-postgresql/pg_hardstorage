// llm_history.go — CLI surface for inspecting and shredding stored LLM session history.
package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/history"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/skills"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/llmprovider"
)

// openHistoryWriter resolves a per-principal DEK and opens
// a writer scoped to (principal, skill).  Returns
// (nil, nil-closer, nil) when history is disabled (operator
// passed --no-history, or no key material is available);
// the chat session continues but no transcript is recorded.
//
// DEK resolution chain:
//
//  1. --history-key-file <path>: read 32-byte hex from
//     the file.  Opaque key — operators using this path
//     manage rotation themselves.
//  2. local KEK at <keyring>/kek.bin: HKDF-derive the DEK.
//     Default for operators with the standard local
//     keyring.  No new key material to track.
//  3. Neither available: return nil writer; chat runs
//     without history.
//
// Per-principal isolation: the principal is the third arg
// to history.DeriveDEK, so alice and bob get distinct DEKs
// from the same KEK.  A file-system-level read of bob's
// transcripts by alice is locked out by the per-principal
// directory permissions; even if those permissions are
// bypassed, alice can't decrypt without bob's principal-
// derived DEK.
func openHistoryWriter(opts llmChatOptions, skill *skills.Skill, prov llmprovider.Provider) (*history.Writer, func() error, error) {
	if opts.noHistory {
		return nil, func() error { return nil }, nil
	}
	principal := resolvePrincipal(opts.principal)

	stateRoot, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return nil, func() error { return nil }, err
	}
	historyRoot := filepath.Join(stateRoot.State.Value, "llm", "conversations")

	dek, err := resolveHistoryDEK(opts, principal, stateRoot.Keyring.Value)
	if err != nil {
		return nil, func() error { return nil }, err
	}
	if dek == nil {
		return nil, func() error { return nil }, nil // history silently disabled
	}

	store, err := history.New(historyRoot)
	if err != nil {
		return nil, func() error { return nil }, fmt.Errorf("history: open store: %w", err)
	}
	if err := store.SetDEK(dek); err != nil {
		return nil, func() error { return nil }, fmt.Errorf("history: install DEK: %w", err)
	}

	skillName, skillVer := "", ""
	if skill != nil {
		skillName, skillVer = skill.Name, skill.Version
	}
	provName := ""
	if prov != nil {
		provName = prov.Name()
	}
	w, err := store.Open(history.SessionMeta{
		Principal: principal,
		Skill:     skillName,
		SkillVer:  skillVer,
		Provider:  provName,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		return nil, func() error { return nil }, fmt.Errorf("history: open writer: %w", err)
	}
	return w, func() error { return w.Close(0) }, nil
}

// resolveHistoryDEK threads the file > KEK > none chain.
func resolveHistoryDEK(opts llmChatOptions, principal, keyringDir string) ([]byte, error) {
	if opts.historyKeyFile != "" {
		return readHexKeyFile(opts.historyKeyFile)
	}
	if !keystore.KEKExists(keyringDir) {
		return nil, nil // no key material; history skipped
	}
	kek, _, err := keystore.LoadOrGenerateKEK(keyringDir)
	if err != nil {
		return nil, fmt.Errorf("history: read local KEK: %w", err)
	}
	return history.DeriveDEK(kek[:], principal)
}

// readHexKeyFile reads a 32-byte hex key from path.
// Permissive about whitespace + line endings.
func readHexKeyFile(path string) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("history: read key file %s: %w", path, err)
	}
	hexStr := strings.TrimSpace(string(body))
	dek, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, fmt.Errorf("history: decode hex key %s: %w", path, err)
	}
	if len(dek) != 32 {
		return nil, fmt.Errorf("history: key file %s holds %d bytes; want 32", path, len(dek))
	}
	return dek, nil
}

// resolvePrincipal picks the per-user identity string used
// for history isolation.  Order: --principal > $USER >
// "anonymous".
func resolvePrincipal(flag string) string {
	if flag != "" {
		return flag
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "anonymous"
}

// --- pg_hardstorage llm history <list|show|shred> ---------------------

func newLlmHistoryCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "history <list|show|shred>",
		Short: "Inspect / shred encrypted LLM conversation transcripts",
	}
	c.AddCommand(newLlmHistoryListCmd(), newLlmHistoryShowCmd(), newLlmHistoryShredCmd())
	return c
}

func newLlmHistoryListCmd() *cobra.Command {
	var principal string
	c := &cobra.Command{
		Use:          "list",
		Short:        "List recorded sessions for the operator (or all principals with --principal '*')",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			scoped := resolvePrincipal(principal)
			if principal == "*" {
				scoped = ""
			}
			store, err := openHistoryStoreForRead(scoped)
			if err != nil {
				return err
			}
			metas, err := store.List(scoped, "")
			if err != nil {
				return output.NewError("history.list_failed",
					fmt.Sprintf("history list: %v", err)).Wrap(err)
			}
			body := historyListBody{Principal: scoped}
			for _, m := range metas {
				body.Sessions = append(body.Sessions, historyListEntry{
					SessionID: m.SessionID,
					Principal: m.Principal,
					Skill:     m.Skill,
					SkillVer:  m.SkillVer,
					Provider:  m.Provider,
					StartedAt: m.StartedAt.Format(time.RFC3339),
					EndedAt:   m.EndedAt.Format(time.RFC3339),
					Entries:   m.EntryCount,
					Bytes:     m.Bytes,
				})
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
		},
	}
	c.Flags().StringVar(&principal, "principal", "",
		"operator principal to scope to (default: $USER); pass '*' to list every principal on the host")
	return c
}

func newLlmHistoryShowCmd() *cobra.Command {
	var (
		principal string
		sessionID string
	)
	c := &cobra.Command{
		Use:          "show <session-id>",
		Short:        "Decrypt and print one session's transcript",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := DispatcherFrom(cmd)
			sessionID = args[0]
			scoped := resolvePrincipal(principal)
			store, err := openHistoryStoreForRead(scoped)
			if err != nil {
				return err
			}
			entries, meta, err := store.Read(scoped, "", sessionID)
			if err != nil {
				return output.NewError("history.show_failed",
					fmt.Sprintf("history show: %v", err)).Wrap(err)
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(historyShowBody{
				Meta:    meta,
				Entries: entries,
			}))
		},
	}
	c.Flags().StringVar(&principal, "principal", "",
		"operator principal (default: $USER)")
	return c
}

func newLlmHistoryShredCmd() *cobra.Command {
	var (
		principal string
		yes       bool
	)
	c := &cobra.Command{
		Use:          "shred",
		Short:        "Delete every session for the principal (irreversible without backups)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			if !yes {
				return output.NewError("usage.confirmation_required",
					"history shred: --yes is REQUIRED — this deletes every session under the principal").
					Wrap(output.ErrUsage)
			}
			scoped := resolvePrincipal(principal)
			store, err := openHistoryStoreForRead(scoped)
			if err != nil {
				return err
			}
			count, err := store.Shred(scoped, "")
			if err != nil {
				return output.NewError("history.shred_failed",
					fmt.Sprintf("history shred: %v", err)).Wrap(err)
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(historyShredBody{
				Principal: scoped,
				Removed:   count,
			}))
		},
	}
	c.Flags().StringVar(&principal, "principal", "",
		"operator principal (default: $USER)")
	c.Flags().BoolVar(&yes, "yes", false,
		"acknowledge that the deletion is irreversible (no backup of these transcripts is taken anywhere — they live only on the operator's host)")
	return c
}

// openHistoryStoreForRead is the shared open path for the
// list/show/shred commands.  Resolves the same DEK chain
// the chat session uses; returns ErrNoDEK / ErrNotFound as
// structured CLI errors.
func openHistoryStoreForRead(principal string) (*history.Store, error) {
	stateRoot, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return nil, output.NewError("internal", err.Error()).Wrap(err)
	}
	historyRoot := filepath.Join(stateRoot.State.Value, "llm", "conversations")

	store, err := history.New(historyRoot)
	if err != nil {
		return nil, output.NewError("history.open_failed",
			fmt.Sprintf("history: %v", err)).Wrap(err)
	}
	if !keystore.KEKExists(stateRoot.Keyring.Value) {
		return nil, output.NewError("history.no_dek",
			"history: no local KEK at the keyring (history requires a key for at-rest encryption)").
			WithSuggestion(&output.Suggestion{
				Human:   "run `pg_hardstorage init --encrypt` to provision the keyring + KEK; or pass --history-key-file at chat time",
				Command: "pg_hardstorage init --encrypt",
			}).Wrap(history.ErrNoDEK)
	}
	kek, _, err := keystore.LoadOrGenerateKEK(stateRoot.Keyring.Value)
	if err != nil {
		return nil, output.NewError("history.kek_load_failed",
			fmt.Sprintf("history: load KEK: %v", err)).Wrap(err)
	}
	dek, err := history.DeriveDEK(kek[:], principal)
	if err != nil {
		return nil, output.NewError("history.dek_derive_failed",
			fmt.Sprintf("history: derive DEK: %v", err)).Wrap(err)
	}
	if err := store.SetDEK(dek); err != nil {
		return nil, output.NewError("history.set_dek_failed",
			fmt.Sprintf("history: install DEK: %v", err)).Wrap(err)
	}
	return store, nil
}

// --- result bodies ----------------------------------------------------

type historyListBody struct {
	Principal string             `json:"principal,omitempty"`
	Sessions  []historyListEntry `json:"sessions"`
}

type historyListEntry struct {
	SessionID string `json:"session_id"`
	Principal string `json:"principal"`
	Skill     string `json:"skill"`
	SkillVer  string `json:"skill_version"`
	Provider  string `json:"provider"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
	Entries   int    `json:"entries"`
	Bytes     int64  `json:"bytes"`
}

// WriteText renders the session listing as human-readable text to w.
func (b historyListBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if b.Principal != "" {
		fmt.Fprintf(bw, "Sessions for principal %q (oldest first):\n", b.Principal)
	} else {
		fmt.Fprintf(bw, "Sessions across every principal (oldest first):\n")
	}
	if len(b.Sessions) == 0 {
		fmt.Fprintf(bw, "  (none recorded)")
	}
	for _, s := range b.Sessions {
		fmt.Fprintf(bw, "  %s  skill=%s/%s  provider=%s  entries=%d  %s\n",
			s.SessionID, s.Skill, s.SkillVer, s.Provider, s.Entries, s.StartedAt)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type historyShowBody struct {
	Meta    history.SessionMeta `json:"meta"`
	Entries []history.Entry     `json:"entries"`
}

// WriteText renders one session's metadata and entry stream as human-readable
// text to w.
func (b historyShowBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "Session %s\n", b.Meta.SessionID)
	fmt.Fprintf(bw, "  Skill:     %s/%s\n", b.Meta.Skill, b.Meta.SkillVer)
	fmt.Fprintf(bw, "  Provider:  %s\n", b.Meta.Provider)
	fmt.Fprintf(bw, "  Principal: %s\n", b.Meta.Principal)
	fmt.Fprintf(bw, "  Range:     %s → %s\n",
		b.Meta.StartedAt.Format(time.RFC3339),
		b.Meta.EndedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "  Entries:   %d (%d bytes)\n\n", b.Meta.EntryCount, b.Meta.Bytes)
	for _, e := range b.Entries {
		fmt.Fprintf(bw, "[%s] %s.%s  %s\n",
			e.At.Format("15:04:05"), e.Role, e.Op, string(e.Body))
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type historyShredBody struct {
	Principal string `json:"principal"`
	Removed   int    `json:"removed"`
}

// WriteText renders the shred-history result as a single-line confirmation to w.
func (b historyShredBody) WriteText(w io.Writer) error {
	_, err := fmt.Fprintf(w, "✓ history shred — removed %d session(s) for principal %q",
		b.Removed, b.Principal)
	return err
}

// silence unused import warnings for tools we ferry through
// the file's CLI body without referencing inside helpers.
var (
	_ = errors.New
	_ = context.Background
)
