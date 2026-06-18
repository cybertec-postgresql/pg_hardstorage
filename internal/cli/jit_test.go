package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/jit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// jitIssueView mirrors the jitIssueBody surface that the CLI emits.
type jitIssueView struct {
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

type jitListView struct {
	Count   int `json:"count"`
	Entries []struct {
		Token struct {
			ID        string   `json:"id"`
			Principal string   `json:"principal"`
			Scope     []string `json:"scope"`
			Tenant    string   `json:"tenant,omitempty"`
		} `json:"token"`
		EffectiveStatus string `json:"effective_status"`
	} `json:"entries"`
}

type jitShowView struct {
	Token struct {
		ID                   string   `json:"id"`
		Principal            string   `json:"principal"`
		Scope                []string `json:"scope"`
		Tenant               string   `json:"tenant,omitempty"`
		Reason               string   `json:"reason"`
		PublicKeyFingerprint string   `json:"public_key_fingerprint"`
	} `json:"token"`
	Revocation *struct {
		RevokedAt time.Time `json:"revoked_at"`
		RevokedBy string    `json:"revoked_by,omitempty"`
		Reason    string    `json:"reason,omitempty"`
	} `json:"revocation,omitempty"`
	EffectiveStatus string `json:"effective_status"`
}

type jitRevokeView struct {
	ID        string    `json:"id"`
	RevokedAt time.Time `json:"revoked_at"`
	RevokedBy string    `json:"revoked_by,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

type jitVerifyView struct {
	ID              string    `json:"id"`
	Principal       string    `json:"principal"`
	Scope           []string  `json:"scope"`
	Operation       string    `json:"operation"`
	EffectiveStatus string    `json:"effective_status"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// ----- issue -----

func TestJitIssue_RequiresFlags(t *testing.T) {
	w := newReadWorld(t)

	// Missing --repo
	_, errb, exit := runCLI(t, "jit", "issue", "ops@acme.example",
		"--scope", "kms.shred", "--reason", "x", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --repo: exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}

	// Missing --scope
	_, errb, exit = runCLI(t, "jit", "issue", "ops@acme.example",
		"--repo", w.repoURL, "--reason", "x", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --scope: exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}

	// Missing --reason
	_, errb, exit = runCLI(t, "jit", "issue", "ops@acme.example",
		"--repo", w.repoURL, "--scope", "kms.shred", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --reason: exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

func TestJitIssue_BadDuration(t *testing.T) {
	w := newReadWorld(t)
	// 25h > MaxDuration (24h) → ErrInvalidDuration → usage.bad_flag.
	_, errb, exit := runCLI(t, "jit", "issue", "ops@acme.example",
		"--repo", w.repoURL, "--scope", "kms.shred",
		"--reason", "GDPR Art. 17", "--duration", "25h",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestJitIssue_HappyPath(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "jit", "issue", "ops@acme.example",
		"--repo", w.repoURL,
		"--scope", "kms.shred", "--scope", "backup.delete",
		"--reason", "GDPR Art. 17 erasure request #4421",
		"--duration", "1h",
		"--tenant", "default",
		"--issued-by", "alice@example.com",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view jitIssueView
	bodyOf(t, stdout, &view)
	if view.ID == "" {
		t.Errorf("ID empty")
	}
	if view.Token == "" {
		t.Errorf("Token empty (encoded form not emitted)")
	}
	if view.Principal != "ops@acme.example" {
		t.Errorf("Principal = %q, want ops@acme.example", view.Principal)
	}
	if len(view.Scope) != 2 || view.Scope[0] != "kms.shred" || view.Scope[1] != "backup.delete" {
		t.Errorf("Scope = %v", view.Scope)
	}
	if view.Tenant != "default" {
		t.Errorf("Tenant = %q, want default", view.Tenant)
	}
	if view.IssuedBy != "alice@example.com" {
		t.Errorf("IssuedBy = %q", view.IssuedBy)
	}
	if !view.ExpiresAt.After(view.IssuedAt) {
		t.Errorf("ExpiresAt %s not after IssuedAt %s", view.ExpiresAt, view.IssuedAt)
	}
	if d := view.ExpiresAt.Sub(view.IssuedAt); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("duration %s not ~1h", d)
	}
}

// TestJitIssue_TextRender checks the text renderer prints the
// principal + scope + token block (3am-friendly).
func TestJitIssue_TextRender(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "jit", "issue", "ops@acme.example",
		"--repo", w.repoURL,
		"--scope", "kms.shred",
		"--reason", "ad-hoc",
		"--duration", "30m",
		"-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"JIT token issued",
		"ops@acme.example",
		"kms.shred",
		"Token (forward to the principal):",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output missing %q:\n%s", want, stdout)
		}
	}
}

// ----- list -----

func TestJitList_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "jit", "list", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

func TestJitList_BadStatus(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "jit", "list",
		"--repo", w.repoURL, "--status", "exotic",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestJitList_Empty(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "jit", "list",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view jitListView
	bodyOf(t, stdout, &view)
	if view.Count != 0 {
		t.Errorf("Count = %d, want 0", view.Count)
	}
}

func TestJitList_WithFilters(t *testing.T) {
	w := newReadWorld(t)
	// Issue three tokens: ops + admin × default, plus admin × tenantB.
	mustIssue(t, w, "ops@acme.example", "kms.shred", "default")
	mustIssue(t, w, "admin@acme.example", "backup.delete", "default")
	mustIssue(t, w, "admin@acme.example", "kms.shred", "tenant-b")

	// no filter → 3 entries.
	stdout, _, exit := runCLI(t, "jit", "list",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("list-all exit = %d\n%s", exit, stdout)
	}
	var all jitListView
	bodyOf(t, stdout, &all)
	if all.Count != 3 {
		t.Errorf("unfiltered Count = %d, want 3", all.Count)
	}

	// principal filter → 2 admin entries.
	stdout, _, exit = runCLI(t, "jit", "list",
		"--repo", w.repoURL, "--principal", "admin@acme.example",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("principal exit = %d\n%s", exit, stdout)
	}
	var principalScoped jitListView
	bodyOf(t, stdout, &principalScoped)
	if principalScoped.Count != 2 {
		t.Errorf("principal-filter Count = %d, want 2", principalScoped.Count)
	}

	// tenant filter → 1 entry.
	stdout, _, exit = runCLI(t, "jit", "list",
		"--repo", w.repoURL, "--tenant", "tenant-b",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("tenant exit = %d\n%s", exit, stdout)
	}
	var tenantScoped jitListView
	bodyOf(t, stdout, &tenantScoped)
	if tenantScoped.Count != 1 {
		t.Errorf("tenant-filter Count = %d, want 1", tenantScoped.Count)
	}

	// status=active filter → 3 entries (all just-issued).
	stdout, _, exit = runCLI(t, "jit", "list",
		"--repo", w.repoURL, "--status", "active",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("status=active exit = %d\n%s", exit, stdout)
	}
	var activeScoped jitListView
	bodyOf(t, stdout, &activeScoped)
	if activeScoped.Count != 3 {
		t.Errorf("status=active Count = %d, want 3", activeScoped.Count)
	}
}

// ----- show -----

func TestJitShow_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "jit", "show", "anything", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

func TestJitShow_NotFound(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "jit", "show", "no-such-token-id",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound", exit)
	}
	if !strings.Contains(errb, "notfound.token") {
		t.Errorf("expected notfound.token:\n%s", errb)
	}
}

func TestJitShow_HappyPath(t *testing.T) {
	w := newReadWorld(t)
	id := mustIssue(t, w, "ops@acme.example", "kms.shred", "default")

	stdout, _, exit := runCLI(t, "jit", "show", id,
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view jitShowView
	bodyOf(t, stdout, &view)
	if view.Token.ID != id {
		t.Errorf("Token.ID = %q, want %q", view.Token.ID, id)
	}
	if view.Token.Principal != "ops@acme.example" {
		t.Errorf("Principal = %q", view.Token.Principal)
	}
	if view.Token.PublicKeyFingerprint == "" {
		t.Errorf("PublicKeyFingerprint missing")
	}
	if view.EffectiveStatus != string(jit.StatusActive) {
		t.Errorf("EffectiveStatus = %q, want active", view.EffectiveStatus)
	}
	if view.Revocation != nil {
		t.Errorf("Revocation should be nil for an active token")
	}
}

// TestJitShow_AfterRevoke proves show sees the revocation
// marker + reports EffectiveStatus = revoked.
func TestJitShow_AfterRevoke(t *testing.T) {
	w := newReadWorld(t)
	id := mustIssue(t, w, "ops@acme.example", "kms.shred", "")

	_, _, exit := runCLI(t, "jit", "revoke", id,
		"--repo", w.repoURL,
		"--reason", "spotted on the wrong host",
		"--by", "alice@example.com",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("revoke exit = %d", exit)
	}

	stdout, _, exit := runCLI(t, "jit", "show", id,
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("show exit = %d\n%s", exit, stdout)
	}
	var view jitShowView
	bodyOf(t, stdout, &view)
	if view.EffectiveStatus != string(jit.StatusRevoked) {
		t.Errorf("EffectiveStatus = %q, want revoked", view.EffectiveStatus)
	}
	if view.Revocation == nil {
		t.Fatalf("Revocation block missing")
	}
	if view.Revocation.RevokedBy != "alice@example.com" {
		t.Errorf("RevokedBy = %q", view.Revocation.RevokedBy)
	}
	if !strings.Contains(view.Revocation.Reason, "wrong host") {
		t.Errorf("Reason = %q", view.Revocation.Reason)
	}
}

// ----- revoke -----

func TestJitRevoke_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "jit", "revoke", "anything", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

func TestJitRevoke_NotFound(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "jit", "revoke", "no-such-token-id",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound", exit)
	}
	if !strings.Contains(errb, "notfound.token") {
		t.Errorf("expected notfound.token:\n%s", errb)
	}
}

func TestJitRevoke_DoubleRevokeConflict(t *testing.T) {
	w := newReadWorld(t)
	id := mustIssue(t, w, "ops@acme.example", "kms.shred", "")

	// First revoke succeeds.
	_, _, exit := runCLI(t, "jit", "revoke", id,
		"--repo", w.repoURL, "--reason", "first", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("first revoke exit = %d", exit)
	}

	// Second revoke is a conflict.
	_, errb, exit := runCLI(t, "jit", "revoke", id,
		"--repo", w.repoURL, "--reason", "second", "-o", "json")
	if exit != int(output.ExitConflict) {
		t.Errorf("second revoke exit = %d, want ExitConflict", exit)
	}
	if !strings.Contains(errb, "conflict.already_revoked") {
		t.Errorf("expected conflict.already_revoked:\n%s", errb)
	}
}

func TestJitRevoke_HappyPath(t *testing.T) {
	w := newReadWorld(t)
	id := mustIssue(t, w, "ops@acme.example", "kms.shred", "")

	stdout, _, exit := runCLI(t, "jit", "revoke", id,
		"--repo", w.repoURL,
		"--reason", "leaked",
		"--by", "alice@example.com",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view jitRevokeView
	bodyOf(t, stdout, &view)
	if view.ID != id {
		t.Errorf("ID = %q, want %q", view.ID, id)
	}
	if view.RevokedBy != "alice@example.com" {
		t.Errorf("RevokedBy = %q", view.RevokedBy)
	}
	if !strings.Contains(view.Reason, "leaked") {
		t.Errorf("Reason = %q", view.Reason)
	}
	if view.RevokedAt.IsZero() {
		t.Errorf("RevokedAt is zero")
	}
}

// ----- verify -----

func TestJitVerify_RequiresFlags(t *testing.T) {
	w := newReadWorld(t)

	// missing --repo
	_, errb, exit := runCLI(t, "jit", "verify",
		"--token", "x", "--operation", "kms.shred", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}

	// missing --token
	_, errb, exit = runCLI(t, "jit", "verify",
		"--repo", w.repoURL, "--operation", "kms.shred", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}

	// missing --operation
	_, errb, exit = runCLI(t, "jit", "verify",
		"--repo", w.repoURL, "--token", "x", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

func TestJitVerify_BadToken(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "jit", "verify",
		"--repo", w.repoURL,
		"--token", "definitely-not-a-real-token",
		"--operation", "kms.shred",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_token") {
		t.Errorf("expected usage.bad_token:\n%s", errb)
	}
}

func TestJitVerify_HappyPath(t *testing.T) {
	w := newReadWorld(t)

	// Issue a token and capture its encoded form.
	stdout, _, exit := runCLI(t, "jit", "issue", "ops@acme.example",
		"--repo", w.repoURL,
		"--scope", "kms.shred",
		"--reason", "GDPR",
		"--duration", "1h",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("issue exit = %d\n%s", exit, stdout)
	}
	var issued jitIssueView
	bodyOf(t, stdout, &issued)

	// Verify with the matching scope.
	stdout, _, exit = runCLI(t, "jit", "verify",
		"--repo", w.repoURL,
		"--token", issued.Token,
		"--operation", "kms.shred",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("verify exit = %d\n%s", exit, stdout)
	}
	var view jitVerifyView
	bodyOf(t, stdout, &view)
	if view.ID != issued.ID {
		t.Errorf("ID = %q, want %q", view.ID, issued.ID)
	}
	if view.EffectiveStatus != string(jit.StatusActive) {
		t.Errorf("EffectiveStatus = %q, want active", view.EffectiveStatus)
	}
	if view.Operation != "kms.shred" {
		t.Errorf("Operation = %q", view.Operation)
	}
}

func TestJitVerify_ScopeMismatch(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "jit", "issue", "ops@acme.example",
		"--repo", w.repoURL,
		"--scope", "kms.shred",
		"--reason", "GDPR",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("issue exit = %d", exit)
	}
	var issued jitIssueView
	bodyOf(t, stdout, &issued)

	_, errb, exit := runCLI(t, "jit", "verify",
		"--repo", w.repoURL,
		"--token", issued.Token,
		"--operation", "backup.delete",
		"-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("exit = %d, want ExitVerifyFailed", exit)
	}
	if !strings.Contains(errb, "verify.token_invalid") {
		t.Errorf("expected verify.token_invalid:\n%s", errb)
	}
}

func TestJitVerify_AfterRevoke(t *testing.T) {
	w := newReadWorld(t)

	stdout, _, exit := runCLI(t, "jit", "issue", "ops@acme.example",
		"--repo", w.repoURL,
		"--scope", "kms.shred",
		"--reason", "GDPR",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("issue exit = %d", exit)
	}
	var issued jitIssueView
	bodyOf(t, stdout, &issued)

	_, _, exit = runCLI(t, "jit", "revoke", issued.ID,
		"--repo", w.repoURL, "--reason", "no-longer-needed", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("revoke exit = %d", exit)
	}

	_, errb, exit := runCLI(t, "jit", "verify",
		"--repo", w.repoURL,
		"--token", issued.Token,
		"--operation", "kms.shred",
		"-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("exit = %d, want ExitVerifyFailed", exit)
	}
	if !strings.Contains(errb, "verify.token_invalid") {
		t.Errorf("expected verify.token_invalid:\n%s", errb)
	}
}

// TestJitVerify_TenantContextRequired enforces that a tenant-
// scoped token cannot be used outside its tenant.
func TestJitVerify_TenantContextRequired(t *testing.T) {
	w := newReadWorld(t)

	stdout, _, exit := runCLI(t, "jit", "issue", "ops@acme.example",
		"--repo", w.repoURL,
		"--scope", "kms.shred",
		"--reason", "tenant-scoped",
		"--tenant", "tenant-a",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("issue exit = %d", exit)
	}
	var issued jitIssueView
	bodyOf(t, stdout, &issued)

	// Verify with the wrong tenant context → fails.
	_, errb, exit := runCLI(t, "jit", "verify",
		"--repo", w.repoURL,
		"--token", issued.Token,
		"--operation", "kms.shred",
		"--tenant", "tenant-b",
		"-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("wrong-tenant exit = %d, want ExitVerifyFailed", exit)
	}
	if !strings.Contains(errb, "verify.token_invalid") {
		t.Errorf("expected verify.token_invalid:\n%s", errb)
	}

	// Verify with the right tenant context → ok.
	_, _, exit = runCLI(t, "jit", "verify",
		"--repo", w.repoURL,
		"--token", issued.Token,
		"--operation", "kms.shred",
		"--tenant", "tenant-a",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Errorf("right-tenant exit = %d, want ExitOK", exit)
	}
}

// TestJit_HelpDiscoverable: the parent command renders help that
// names every subcommand.
func TestJit_HelpDiscoverable(t *testing.T) {
	stdout, _, exit := runCLI(t, "jit", "--help")
	if exit != int(output.ExitOK) {
		t.Fatalf("help exit = %d\n%s", exit, stdout)
	}
	for _, want := range []string{"issue", "list", "show", "revoke", "verify"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help missing subcommand %q:\n%s", want, stdout)
		}
	}
}

// mustIssue is a tiny helper that issues a token and returns
// its ID.  Keeps individual tests legible.
func mustIssue(t *testing.T, w *readWorld, principal, scope, tenant string) string {
	t.Helper()
	args := []string{"jit", "issue", principal,
		"--repo", w.repoURL,
		"--scope", scope,
		"--reason", "test",
		"-o", "json"}
	if tenant != "" {
		args = append(args, "--tenant", tenant)
	}
	stdout, _, exit := runCLI(t, args...)
	if exit != int(output.ExitOK) {
		t.Fatalf("issue exit = %d\n%s", exit, stdout)
	}
	var view jitIssueView
	bodyOf(t, stdout, &view)
	return view.ID
}

// _ ensures the context import lands even if no test happens to
// reference it directly.
var _ = context.Background
