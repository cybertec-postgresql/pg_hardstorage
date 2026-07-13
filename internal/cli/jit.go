// jit.go — CLI surface for just-in-time access tokens (issue/list/show/revoke/verify).
package cli

import (
	"crypto/ed25519"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/jit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// newJitCmd is the parent command for JIT (just-in-time) access
// tokens.  Subcommands cover the lifecycle:
//
//   - issue   — mint a new token
//   - list    — show every token + lifecycle status
//   - show    — read one token's full body
//   - revoke  — write a revocation marker
//   - verify  — assert a supplied token is currently valid
//
// JIT tokens are ed25519-signed, time-bound grants for break-
// glass operations.  Audit-stamped on issuance + on revocation.
func newJitCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "jit",
		Short: "Just-in-time access tokens for break-glass operations",
		Long: `Manage JIT (just-in-time) access tokens.

JIT tokens are time-bound elevated grants signed with the
operator's ed25519 signing key.  Use case: an operator under
supervised access needs to perform a destructive operation
(kms shred, repo gc --apply, etc.) without holding a permanent
elevated token.  An admin issues a short-TTL token scoped to
the specific operation; the operator passes the token to the
destructive command; the audit chain records both issuance and
consumption.

Operationally:

    # admin issues a 1-hour token for a single shred
    pg_hardstorage jit issue ops@acme.example \
        --scope kms.shred --duration 1h \
        --reason "GDPR Art. 17 erasure request #4421" \
        --repo s3://acme

    # operator consumes (future commit will wire --jit-token
    # into kms shred + other destructive commands)
    pg_hardstorage kms shred --repo s3://acme \
        --confirm-keyring <keyring-dir> --require-approval <id> --yes

    # admin lists current tokens
    pg_hardstorage jit list --repo s3://acme

    # admin revokes if needed
    pg_hardstorage jit revoke <token-id> --reason "..."`,
	}
	c.AddCommand(newJitIssueCmd())
	c.AddCommand(newJitListCmd())
	c.AddCommand(newJitShowCmd())
	c.AddCommand(newJitRevokeCmd())
	c.AddCommand(newJitVerifyCmd())
	return c
}

func newJitIssueCmd() *cobra.Command {
	var (
		repoURL  string
		scope    []string
		reason   string
		duration time.Duration
		tenant   string
		issuedBy string
	)
	c := &cobra.Command{
		Use:   "issue <principal>",
		Short: "Mint a new JIT access token",
		Long: `Mint a new ed25519-signed JIT token for <principal>.  The
token is persisted at jit/<id>.json in the repo + audit-stamped.
The encoded token is printed to stdout for the operator to
forward to the principal.

Required flags:
  --scope LIST       comma-separated list of scopes (e.g. kms.shred,backup.delete)
  --reason "..."     operator-supplied justification (required)
  --duration DUR     token TTL (1m..24h)

Optional:
  --tenant T         scope the token to one tenant
  --issued-by NAME   record the issuer's identity in the manifest`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJitIssue(cmd, args[0], jitIssueFlags{
				repoURL: repoURL, scope: scope, reason: reason,
				duration: duration, tenant: tenant, issuedBy: issuedBy,
			})
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringSliceVar(&scope, "scope", nil,
		"scope (repeatable; e.g. --scope kms.shred --scope backup.delete) (required)")
	_ = c.MarkFlagRequired("scope")
	c.Flags().StringVar(&reason, "reason", "", "operator-supplied justification (required)")
	_ = c.MarkFlagRequired("reason")
	c.Flags().DurationVar(&duration, "duration", time.Hour,
		"token TTL (1m..24h); default 1h")
	c.Flags().StringVar(&tenant, "tenant", "", "scope the token to one tenant")
	c.Flags().StringVar(&issuedBy, "issued-by", "", "record the issuer's identity")
	return c
}

type jitIssueFlags struct {
	repoURL  string
	scope    []string
	reason   string
	duration time.Duration
	tenant   string
	issuedBy string
}

func runJitIssue(cmd *cobra.Command, principal string, f jitIssueFlags) error {
	d := DispatcherFrom(cmd)
	// --scope required-ness is declared via MarkFlagRequired.

	bsigner, _, err := loadSignerForJIT()
	if err != nil {
		return err
	}
	repoMeta, sp, err := openRepo(cmd.Context(), f.repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	_ = repoMeta

	signer := jitSignerAdapter{s: bsigner}
	tok, err := jit.Issue(signer, jit.IssueOptions{
		Principal: principal,
		Scope:     f.scope,
		Reason:    f.reason,
		Duration:  f.duration,
		Tenant:    f.tenant,
		IssuedBy:  f.issuedBy,
	})
	if err != nil {
		switch {
		case errors.Is(err, jit.ErrInvalidDuration),
			errors.Is(err, jit.ErrInvalidScope),
			errors.Is(err, jit.ErrReasonTooLong),
			errors.Is(err, jit.ErrPrincipalRequired),
			errors.Is(err, jit.ErrMissingReason):
			return output.NewError("usage.bad_flag",
				fmt.Sprintf("jit issue: %v", err)).Wrap(output.ErrUsage)
		}
		return output.NewError("jit.issue_failed",
			fmt.Sprintf("jit issue: %v", err)).Wrap(err)
	}

	store := jit.NewStore(sp)
	if err := store.Put(cmd.Context(), tok); err != nil {
		return output.NewError("jit.store_failed",
			fmt.Sprintf("jit issue: store: %v", err)).Wrap(err)
	}

	encoded, err := jit.Encode(tok)
	if err != nil {
		return output.NewError("internal",
			fmt.Sprintf("jit issue: encode: %v", err)).Wrap(err)
	}

	// Best-effort audit.  A failed audit append doesn't roll
	// back the issuance — the token is signed + persisted; the
	// auditor sees the issuance via the storage layer even if
	// the chain entry is missing (operators who care about
	// chain-integrity for issuance run audit verify-chain
	// periodically).
	auditStore := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	_ = auditStore.Append(cmd.Context(), &audit.Event{
		Action:    "jit.issue",
		Subject:   audit.Subject{Tenant: f.tenant},
		Tenant:    f.tenant,
		Actor:     f.issuedBy,
		Timestamp: time.Now().UTC(),
		Body: map[string]any{
			"token_id":   tok.ID,
			"principal":  tok.Principal,
			"scope":      tok.Scope,
			"reason":     tok.Reason,
			"expires_at": tok.ExpiresAt,
		},
	})

	body := jitIssueBody{
		ID:        tok.ID,
		Token:     encoded,
		Principal: tok.Principal,
		Scope:     tok.Scope,
		IssuedAt:  tok.IssuedAt,
		ExpiresAt: tok.ExpiresAt,
		Tenant:    tok.Tenant,
		IssuedBy:  tok.IssuedBy,
		Reason:    tok.Reason,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

type jitIssueBody struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	Principal string    `json:"principal"`
	Scope     []string  `json:"scope"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Tenant    string    `json:"tenant,omitempty"`
	IssuedBy  string    `json:"issued_by,omitempty"`
	Reason    string    `json:"reason"`
}

// WriteText renders the issued token — metadata plus the encoded token
// string — as human-readable text to w.
func (b jitIssueBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ JIT token issued\n")
	fmt.Fprintf(bw, "  ID:        %s\n", b.ID)
	fmt.Fprintf(bw, "  Principal: %s\n", b.Principal)
	fmt.Fprintf(bw, "  Scope:     %s\n", strings.Join(b.Scope, ", "))
	if b.Tenant != "" {
		fmt.Fprintf(bw, "  Tenant:    %s\n", b.Tenant)
	}
	fmt.Fprintf(bw, "  Issued at: %s\n", b.IssuedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "  Expires at:%s\n", b.ExpiresAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "  Reason:    %s\n", b.Reason)
	fmt.Fprintln(bw)
	fmt.Fprintln(bw, "Token (forward to the principal):")
	fmt.Fprintln(bw, b.Token)
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

func newJitListCmd() *cobra.Command {
	var (
		repoURL   string
		principal string
		statusF   string
		tenant    string
	)
	c := &cobra.Command{
		Use:          "list",
		Short:        "List JIT tokens with effective lifecycle status",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runJitList(cmd, repoURL, principal, statusF, tenant)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&principal, "principal", "", "filter by principal")
	c.Flags().StringVar(&statusF, "status", "",
		"filter by status: active | not_yet_active | expired | revoked")
	c.Flags().StringVar(&tenant, "tenant", "", "filter by tenant")
	return c
}

func runJitList(cmd *cobra.Command, repoURL, principal, statusF, tenant string) error {
	d := DispatcherFrom(cmd)
	var status jit.Status
	switch strings.ToLower(statusF) {
	case "":
	case "active":
		status = jit.StatusActive
	case "not_yet_active":
		status = jit.StatusNotYetActive
	case "expired":
		status = jit.StatusExpired
	case "revoked":
		status = jit.StatusRevoked
	default:
		return output.NewError("usage.bad_flag",
			fmt.Sprintf("jit list: unknown --status %q", statusF)).Wrap(output.ErrUsage)
	}
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	entries, err := jit.NewStore(sp).List(cmd.Context(), jit.ListFilter{
		Principal: principal,
		Status:    status,
		Tenant:    tenant,
	})
	if err != nil {
		return output.NewError("jit.list_failed",
			fmt.Sprintf("jit list: %v", err)).Wrap(err)
	}
	body := jitListBody{Count: len(entries), Entries: entries}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

type jitListBody struct {
	Count   int              `json:"count"`
	Entries []*jit.ListEntry `json:"entries"`
}

// WriteText renders the JIT token list with effective lifecycle status as a
// tabular summary to w.
func (b jitListBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "%d token(s)\n\n", b.Count)
	if b.Count == 0 {
		_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
		return err
	}
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPRINCIPAL\tSCOPE\tSTATUS\tEXPIRES")
	for _, e := range b.Entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			e.Token.ID, e.Token.Principal,
			strings.Join(e.Token.Scope, ","),
			e.EffectiveStatus,
			e.Token.ExpiresAt.Format(time.RFC3339))
	}
	_ = tw.Flush()
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

func newJitShowCmd() *cobra.Command {
	var repoURL string
	c := &cobra.Command{
		Use:          "show <token-id>",
		Short:        "Show one JIT token's full body + revocation status",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJitShow(cmd, repoURL, args[0])
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	return c
}

func runJitShow(cmd *cobra.Command, repoURL, id string) error {
	d := DispatcherFrom(cmd)
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	store := jit.NewStore(sp)
	tok, err := store.Get(cmd.Context(), id)
	if err != nil {
		if errors.Is(err, jit.ErrTokenNotFound) {
			return output.NewError("notfound.token",
				fmt.Sprintf("jit show: token %q not found", id)).Wrap(err)
		}
		return output.NewError("jit.get_failed",
			fmt.Sprintf("jit show: %v", err)).Wrap(err)
	}
	revocation, _ := store.GetRevocation(cmd.Context(), id)
	body := jitShowBody{
		Token:           tok,
		Revocation:      revocation,
		EffectiveStatus: computeShowStatus(tok, revocation != nil),
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

type jitShowBody struct {
	Token           *jit.Token      `json:"token"`
	Revocation      *jit.Revocation `json:"revocation,omitempty"`
	EffectiveStatus jit.Status      `json:"effective_status"`
}

// WriteText renders the full token body plus revocation status as
// human-readable text to w.
func (b jitShowBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	t := b.Token
	fmt.Fprintf(bw, "Token %s\n", t.ID)
	fmt.Fprintf(bw, "  Principal:           %s\n", t.Principal)
	fmt.Fprintf(bw, "  Scope:               %s\n", strings.Join(t.Scope, ", "))
	if t.Tenant != "" {
		fmt.Fprintf(bw, "  Tenant:              %s\n", t.Tenant)
	}
	fmt.Fprintf(bw, "  Issued at:           %s\n", t.IssuedAt.Format(time.RFC3339))
	fmt.Fprintf(bw, "  Expires at:          %s\n", t.ExpiresAt.Format(time.RFC3339))
	if t.IssuedBy != "" {
		fmt.Fprintf(bw, "  Issued by:           %s\n", t.IssuedBy)
	}
	fmt.Fprintf(bw, "  Reason:              %s\n", t.Reason)
	fmt.Fprintf(bw, "  Public-key SHA-256:  %s\n", t.PublicKeyFingerprint)
	fmt.Fprintf(bw, "  Effective status:    %s\n", b.EffectiveStatus)
	if b.Revocation != nil {
		fmt.Fprintln(bw)
		fmt.Fprintln(bw, "Revocation:")
		fmt.Fprintf(bw, "  Revoked at: %s\n", b.Revocation.RevokedAt.Format(time.RFC3339))
		if b.Revocation.RevokedBy != "" {
			fmt.Fprintf(bw, "  Revoked by: %s\n", b.Revocation.RevokedBy)
		}
		if b.Revocation.Reason != "" {
			fmt.Fprintf(bw, "  Reason:     %s\n", b.Revocation.Reason)
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

func computeShowStatus(t *jit.Token, revoked bool) jit.Status {
	if revoked {
		return jit.StatusRevoked
	}
	return t.Status(time.Now().UTC())
}

func newJitRevokeCmd() *cobra.Command {
	var (
		repoURL string
		reason  string
		by      string
	)
	c := &cobra.Command{
		Use:          "revoke <token-id>",
		Short:        "Revoke a JIT token (writes a sibling .revoked marker)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJitRevoke(cmd, repoURL, args[0], by, reason)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&reason, "reason", "", "revocation reason (recorded in the marker)")
	c.Flags().StringVar(&by, "by", "", "operator identity recording the revocation")
	return c
}

func runJitRevoke(cmd *cobra.Command, repoURL, id, by, reason string) error {
	d := DispatcherFrom(cmd)
	repoMeta, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	store := jit.NewStore(sp)
	now := time.Now().UTC()
	if err := store.Revoke(cmd.Context(), id, by, reason, now); err != nil {
		switch {
		case errors.Is(err, jit.ErrTokenNotFound):
			return output.NewError("notfound.token",
				fmt.Sprintf("jit revoke: token %q not found", id)).Wrap(err)
		case errors.Is(err, jit.ErrAlreadyRevoked):
			return output.NewError("conflict.already_revoked",
				fmt.Sprintf("jit revoke: token %q already revoked", id)).Wrap(err)
		}
		return output.NewError("jit.revoke_failed",
			fmt.Sprintf("jit revoke: %v", err)).Wrap(err)
	}
	// Best-effort audit.
	auditStore := audit.NewStoreWithRetention(sp, repoMeta.WORM)
	_ = auditStore.Append(cmd.Context(), &audit.Event{
		Action:    "jit.revoke",
		Actor:     by,
		Timestamp: now,
		Body: map[string]any{
			"token_id": id,
			"reason":   reason,
		},
	})
	body := jitRevokeBody{ID: id, RevokedAt: now, RevokedBy: by, Reason: reason}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

type jitRevokeBody struct {
	ID        string    `json:"id"`
	RevokedAt time.Time `json:"revoked_at"`
	RevokedBy string    `json:"revoked_by,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// WriteText renders the revoke confirmation as human-readable text to w.
func (b jitRevokeBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ Token %s revoked at %s\n", b.ID, b.RevokedAt.Format(time.RFC3339))
	if b.RevokedBy != "" {
		fmt.Fprintf(bw, "  Revoked by: %s\n", b.RevokedBy)
	}
	if b.Reason != "" {
		fmt.Fprintf(bw, "  Reason: %s\n", b.Reason)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

func newJitVerifyCmd() *cobra.Command {
	var (
		repoURL   string
		token     string
		operation string
		tenant    string
	)
	c := &cobra.Command{
		Use:   "verify",
		Short: "Verify a supplied JIT token is currently valid for an operation",
		Long: `Decodes + validates a JIT token: signature, expiration,
revocation, scope, tenant.  Returns the token body on success;
exit 9 (verify-failed) on any failure.

Use this manually before running a destructive command to confirm
the token is still good.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runJitVerify(cmd, repoURL, token, operation, tenant)
		},
	}
	c.Flags().StringVar(&repoURL, "repo", "", "repository URL (required)")
	_ = c.MarkFlagRequired("repo")
	c.Flags().StringVar(&token, "token", "",
		"the encoded token (base64url; required)")
	_ = c.MarkFlagRequired("token")
	c.Flags().StringVar(&operation, "operation", "",
		"operation to authorise; matched against scope (required)")
	_ = c.MarkFlagRequired("operation")
	c.Flags().StringVar(&tenant, "tenant", "",
		"tenant context for the verification")
	return c
}

func runJitVerify(cmd *cobra.Command, repoURL, encoded, operation, tenant string) error {
	d := DispatcherFrom(cmd)
	tok, err := jit.Decode(encoded)
	if err != nil {
		return output.NewError("usage.bad_token",
			fmt.Sprintf("jit verify: %v", err)).Wrap(output.ErrUsage)
	}
	bsigner, _, err := loadSignerForJIT()
	if err != nil {
		return err
	}
	_, sp, err := openRepo(cmd.Context(), repoURL)
	if err != nil {
		return err
	}
	defer sp.Close()
	resolver := &jit.SingleKeyResolver{Key: bsigner.PublicKey()}
	if err := jit.VerifyAt(cmd.Context(), jit.NewStore(sp), resolver, tok, jit.CheckOptions{
		Operation: operation,
		Tenant:    tenant,
	}); err != nil {
		return output.NewError("verify.token_invalid",
			fmt.Sprintf("jit verify: %v", err)).
			WithSuggestion(&output.Suggestion{
				Human: "the token didn't pass one or more checks (signature / expiration / revocation / scope / tenant); see the message above",
			})
	}
	body := jitVerifyBody{
		ID:              tok.ID,
		Principal:       tok.Principal,
		Scope:           tok.Scope,
		Operation:       operation,
		EffectiveStatus: jit.StatusActive,
		ExpiresAt:       tok.ExpiresAt,
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

type jitVerifyBody struct {
	ID              string     `json:"id"`
	Principal       string     `json:"principal"`
	Scope           []string   `json:"scope"`
	Operation       string     `json:"operation"`
	EffectiveStatus jit.Status `json:"effective_status"`
	ExpiresAt       time.Time  `json:"expires_at"`
}

// WriteText renders the verify-token outcome as human-readable text to w.
func (b jitVerifyBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ Token %s is valid for operation %q\n", b.ID, b.Operation)
	fmt.Fprintf(bw, "  Principal:  %s\n", b.Principal)
	fmt.Fprintf(bw, "  Expires at: %s\n", b.ExpiresAt.Format(time.RFC3339))
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// loadSignerForJIT loads the operator's signing keypair via the
// canonical keystore path.  Same path the rest of the binary
// uses; ensures JIT tokens are signed by the same key as
// manifests + audit bundles.
func loadSignerForJIT() (*backup.Signer, *backup.Verifier, error) {
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return nil, nil, output.NewError("internal", err.Error()).Wrap(err)
	}
	signer, verifier, err := keystore.LoadOrGenerate(p.Keyring.Value)
	if err != nil {
		return nil, nil, output.NewError("internal",
			fmt.Sprintf("jit: load signer: %v", err)).Wrap(err)
	}
	return signer, verifier, nil
}

// jitSignerAdapter wraps backup.Signer to satisfy jit.Signer
// (avoids the jit → backup import cycle).
type jitSignerAdapter struct {
	s *backup.Signer
}

// Sign returns the ed25519 signature of payload using the wrapped backup signer.
func (a jitSignerAdapter) Sign(payload []byte) []byte { return a.s.Sign(payload) }

// PublicKey returns the ed25519 public key of the wrapped backup signer.
func (a jitSignerAdapter) PublicKey() ed25519.PublicKey { return a.s.PublicKey() }

// jitJSON is a tiny helper just to anchor the encoding/json
// import via a typed value.  Without it gofmt would prune the
// import even though we use it via stdjson.Marshal in the body
// renderers.
var _ = stdjson.Marshal
