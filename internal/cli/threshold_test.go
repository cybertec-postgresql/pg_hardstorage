package cli_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	stdjson "encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/threshold"
)

// thresholdWhoamiView mirrors the whoami body shape.
type thresholdWhoamiView struct {
	Fingerprint    string `json:"fingerprint"`
	PublicKey      string `json:"public_key"`
	MemberSpecHint string `json:"member_spec_hint"`
}

type thresholdRosterMemberView struct {
	Signer               string `json:"signer"`
	PublicKeyFingerprint string `json:"public_key_fingerprint"`
}

type thresholdRosterView struct {
	ID                          string                      `json:"id"`
	Description                 string                      `json:"description,omitempty"`
	Threshold                   int                         `json:"threshold"`
	Members                     []thresholdRosterMemberView `json:"members"`
	CreatedBy                   string                      `json:"created_by"`
	CreatorPublicKeyFingerprint string                      `json:"creator_public_key_fingerprint"`
	Hash                        string                      `json:"hash"`
}

type thresholdRosterListView struct {
	Count   int                   `json:"count"`
	Entries []thresholdRosterView `json:"entries"`
}

type thresholdSignView struct {
	Signer               string                       `json:"signer"`
	PublicKeyFingerprint string                       `json:"public_key_fingerprint"`
	Subject              threshold.AttestationSubject `json:"subject"`
	RosterID             string                       `json:"roster_id"`
}

type thresholdVerifyView struct {
	Subject         threshold.AttestationSubject `json:"subject"`
	RosterID        string                       `json:"roster_id"`
	Threshold       int                          `json:"threshold"`
	Signatures      int                          `json:"signatures"`
	ValidSignatures int                          `json:"valid_signatures"`
	Met             bool                         `json:"met"`
}

type thresholdShowView struct {
	Header struct {
		Subject  threshold.AttestationSubject `json:"subject"`
		RosterID string                       `json:"roster_id"`
	} `json:"header"`
	Signatures []struct {
		Signer string `json:"signer"`
		Valid  bool   `json:"valid"`
	} `json:"signatures"`
	Threshold       int  `json:"threshold,omitempty"`
	ValidDistinct   int  `json:"valid_distinct,omitempty"`
	QuorumMet       bool `json:"quorum_met"`
	RosterAvailable bool `json:"roster_available"`
}

// localFingerprint reads the local keystore's public key fingerprint
// the same way the CLI does, so test expectations track the CLI's
// idea of "who am I".
func localFingerprint(t *testing.T) (fp string, pubKeyB64 string) {
	t.Helper()
	stdout, _, exit := runCLI(t, "threshold", "whoami", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("whoami exit = %d\n%s", exit, stdout)
	}
	var v thresholdWhoamiView
	bodyOf(t, stdout, &v)
	return v.Fingerprint, v.PublicKey
}

// generateOutOfBandMember builds a Member spec for a synthetic
// non-local operator (e.g. bob).  Returns the spec string and the
// raw private key so we can later sign on bob's behalf via the
// lower-level threshold API.
func generateOutOfBandMember(t *testing.T, signerID string) (memberSpec string,
	pubKey ed25519.PublicKey, privKey ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spec := fmt.Sprintf("%s:%s", signerID, base64.StdEncoding.EncodeToString(pub))
	return spec, pub, priv
}

// ----- whoami -----

func TestThresholdWhoami_Happy(t *testing.T) {
	_ = newReadWorld(t)
	stdout, _, exit := runCLI(t, "threshold", "whoami", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v thresholdWhoamiView
	bodyOf(t, stdout, &v)
	if len(v.Fingerprint) != 16 {
		t.Errorf("Fingerprint length = %d, want 16", len(v.Fingerprint))
	}
	if v.PublicKey == "" {
		t.Errorf("PublicKey empty")
	}
	if !strings.Contains(v.MemberSpecHint, ":") {
		t.Errorf("MemberSpecHint missing colon: %q", v.MemberSpecHint)
	}
}

func TestThresholdWhoami_TextRender(t *testing.T) {
	_ = newReadWorld(t)
	stdout, _, exit := runCLI(t, "threshold", "whoami", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"Public-key SHA-256:",
		"Public key (base64):",
		"--member spec",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text missing %q:\n%s", want, stdout)
		}
	}
}

// ----- roster create -----

func TestThresholdRosterCreate_RequiresFlags(t *testing.T) {
	w := newReadWorld(t)
	_, pub := localFingerprint(t)

	cases := []struct {
		name string
		args []string
	}{
		{"missing --repo", []string{"threshold", "roster", "create", "x",
			"--threshold", "1",
			"--member", "alice@e:" + pub,
			"-o", "json"}},
		{"missing --threshold", []string{"threshold", "roster", "create", "x",
			"--repo", w.repoURL,
			"--member", "alice@e:" + pub,
			"-o", "json"}},
		{"missing --member/--members-file", []string{"threshold", "roster", "create", "x",
			"--repo", w.repoURL, "--threshold", "1",
			"-o", "json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errb, exit := runCLI(t, tc.args...)
			if exit != int(output.ExitMisuse) {
				t.Errorf("exit = %d, want ExitMisuse", exit)
			}
			if !strings.Contains(errb, "usage.missing_flag") {
				t.Errorf("expected usage.missing_flag:\n%s", errb)
			}
		})
	}
}

func TestThresholdRosterCreate_BothMemberAndFile(t *testing.T) {
	w := newReadWorld(t)
	_, _, exit := runCLI(t, "threshold", "roster", "create", "x",
		"--repo", w.repoURL, "--threshold", "1",
		"--member", "alice@e:abc",
		"--members-file", "/dev/null",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
}

func TestThresholdRosterCreate_BadMemberSpec(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "threshold", "roster", "create", "x",
		"--repo", w.repoURL, "--threshold", "1",
		"--member", "missing-colon",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestThresholdRosterCreate_HappySingleMember(t *testing.T) {
	w := newReadWorld(t)
	_, pub := localFingerprint(t)

	stdout, _, exit := runCLI(t, "threshold", "roster", "create", "prod-admins",
		"--repo", w.repoURL,
		"--threshold", "1",
		"--member", "alice@acme.example:"+pub,
		"--description", "Production admins",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v thresholdRosterView
	bodyOf(t, stdout, &v)
	if v.ID != "prod-admins" {
		t.Errorf("ID = %q", v.ID)
	}
	if v.Threshold != 1 || len(v.Members) != 1 {
		t.Errorf("Threshold/Members = %d / %d", v.Threshold, len(v.Members))
	}
	if v.CreatedBy != "alice@acme.example" {
		t.Errorf("CreatedBy = %q (expected alice — inferred from local key match)", v.CreatedBy)
	}
	if v.Hash == "" {
		t.Errorf("Hash empty")
	}
}

func TestThresholdRosterCreate_FromFile(t *testing.T) {
	w := newReadWorld(t)
	_, pub := localFingerprint(t)

	dir := t.TempDir()
	file := filepath.Join(dir, "members.json")
	body := fmt.Sprintf(`[{"signer":"alice@acme.example","public_key":%q}]`, pub)
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, exit := runCLI(t, "threshold", "roster", "create", "from-file",
		"--repo", w.repoURL,
		"--threshold", "1",
		"--members-file", file,
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
}

func TestThresholdRosterCreate_LocalKeyNotInMembers(t *testing.T) {
	w := newReadWorld(t)
	// Roster excludes local key → --created-by inference fails →
	// usage.bad_flag.
	memberSpec, _, _ := generateOutOfBandMember(t, "bob@acme.example")
	_, errb, exit := runCLI(t, "threshold", "roster", "create", "no-local",
		"--repo", w.repoURL, "--threshold", "1",
		"--member", memberSpec,
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestThresholdRosterCreate_Conflict(t *testing.T) {
	w := newReadWorld(t)
	_, pub := localFingerprint(t)
	args := []string{"threshold", "roster", "create", "twice",
		"--repo", w.repoURL,
		"--threshold", "1",
		"--member", "alice@acme.example:" + pub,
		"-o", "json"}
	if _, _, exit := runCLI(t, args...); exit != int(output.ExitOK) {
		t.Fatalf("first create exit = %d", exit)
	}
	_, errb, exit := runCLI(t, args...)
	if exit != int(output.ExitConflict) {
		t.Errorf("second create exit = %d, want ExitConflict", exit)
	}
	if !strings.Contains(errb, "conflict.roster_exists") {
		t.Errorf("expected conflict.roster_exists:\n%s", errb)
	}
}

func TestThresholdRosterCreate_BadThreshold(t *testing.T) {
	w := newReadWorld(t)
	_, pub := localFingerprint(t)
	// k=2 with only 1 member → ErrInvalidThreshold → usage.bad_flag.
	_, errb, exit := runCLI(t, "threshold", "roster", "create", "bad-k",
		"--repo", w.repoURL,
		"--threshold", "2",
		"--member", "alice@acme.example:"+pub,
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// ----- roster list / show -----

func TestThresholdRosterList_Empty(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "threshold", "roster", "list",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v thresholdRosterListView
	bodyOf(t, stdout, &v)
	if v.Count != 0 {
		t.Errorf("Count = %d, want 0", v.Count)
	}
}

func TestThresholdRosterList_Happy(t *testing.T) {
	w := newReadWorld(t)
	mustCreateRosterLocal(t, w, "prod-admins", "Production")
	mustCreateRosterLocal(t, w, "staging-ops", "Staging")

	stdout, _, exit := runCLI(t, "threshold", "roster", "list",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v thresholdRosterListView
	bodyOf(t, stdout, &v)
	if v.Count != 2 {
		t.Errorf("Count = %d, want 2", v.Count)
	}
}

func TestThresholdRosterShow_NotFound(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "threshold", "roster", "show", "ghost",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound", exit)
	}
	if !strings.Contains(errb, "notfound.roster") {
		t.Errorf("expected notfound.roster:\n%s", errb)
	}
}

func TestThresholdRosterShow_Happy(t *testing.T) {
	w := newReadWorld(t)
	mustCreateRosterLocal(t, w, "prod-admins", "Production")

	stdout, _, exit := runCLI(t, "threshold", "roster", "show", "prod-admins",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v thresholdRosterView
	bodyOf(t, stdout, &v)
	if v.ID != "prod-admins" {
		t.Errorf("ID = %q", v.ID)
	}
	if v.Description != "Production" {
		t.Errorf("Description = %q", v.Description)
	}
}

// ----- attest sign / verify / show -----

func TestThresholdAttestSign_RequiresFlags(t *testing.T) {
	w := newReadWorld(t)
	cases := []struct {
		name string
		args []string
	}{
		{"--repo", []string{"threshold", "attest", "sign", "k", "i",
			"--hash", "abc", "--roster", "r", "-o", "json"}},
		{"--hash", []string{"threshold", "attest", "sign", "k", "i",
			"--repo", w.repoURL, "--roster", "r", "-o", "json"}},
		{"--roster", []string{"threshold", "attest", "sign", "k", "i",
			"--repo", w.repoURL, "--hash", "abc", "-o", "json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errb, exit := runCLI(t, tc.args...)
			if exit != int(output.ExitMisuse) {
				t.Errorf("exit = %d, want ExitMisuse", exit)
			}
			if !strings.Contains(errb, "usage.missing_flag") {
				t.Errorf("expected usage.missing_flag:\n%s", errb)
			}
		})
	}
}

func TestThresholdAttestSign_RosterNotFound(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "threshold", "attest", "sign",
		"backup_manifest", "db1.full.x",
		"--repo", w.repoURL,
		"--hash", "deadbeef",
		"--roster", "ghost",
		"-o", "json")
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound", exit)
	}
	if !strings.Contains(errb, "notfound.roster") {
		t.Errorf("expected notfound.roster:\n%s", errb)
	}
}

func TestThresholdAttestSign_NotInRoster(t *testing.T) {
	w := newReadWorld(t)
	// Build a roster whose member is NOT the local key.
	memberSpec, _, _ := generateOutOfBandMember(t, "bob@acme.example")
	// Need --created-by because local key doesn't match.
	if _, _, exit := runCLI(t, "threshold", "roster", "create", "no-local",
		"--repo", w.repoURL,
		"--threshold", "1",
		"--member", memberSpec,
		"--created-by", "bob@acme.example",
		"-o", "json"); exit == int(output.ExitOK) {
		t.Fatalf("expected create to fail (creator not in members), got ExitOK")
	}
	// Build it in a way that does succeed: include local key + one
	// out-of-band member.
	_, pub := localFingerprint(t)
	if _, _, exit := runCLI(t, "threshold", "roster", "create", "mixed",
		"--repo", w.repoURL,
		"--threshold", "2",
		"--member", "alice@acme.example:"+pub,
		"--member", memberSpec,
		"-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("expected ExitOK, got %d", exit)
	}
	// Sign with --as bob (not the local key) → key-mismatch.
	_, errb, exit := runCLI(t, "threshold", "attest", "sign",
		"backup_manifest", "db1.full.x",
		"--repo", w.repoURL,
		"--hash", "deadbeef",
		"--roster", "mixed",
		"--as", "bob@acme.example",
		"-o", "json")
	if exit != int(output.ExitAuth) {
		t.Errorf("exit = %d, want ExitAuth", exit)
	}
	if !strings.Contains(errb, "auth.key_mismatch") {
		t.Errorf("expected auth.key_mismatch:\n%s", errb)
	}
}

func TestThresholdAttestSign_HappyPath(t *testing.T) {
	w := newReadWorld(t)
	mustCreateRosterLocal(t, w, "prod-admins", "Production")

	stdout, _, exit := runCLI(t, "threshold", "attest", "sign",
		"backup_manifest", "db1.full.20260501T120000Z",
		"--repo", w.repoURL,
		"--hash", "deadbeefcafebabe",
		"--roster", "prod-admins",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v thresholdSignView
	bodyOf(t, stdout, &v)
	if v.Signer != "alice@acme.example" {
		t.Errorf("Signer = %q", v.Signer)
	}
	if v.Subject.Hash != "deadbeefcafebabe" {
		t.Errorf("Subject.Hash = %q", v.Subject.Hash)
	}
}

func TestThresholdAttestSign_DupNoOp(t *testing.T) {
	w := newReadWorld(t)
	mustCreateRosterLocal(t, w, "prod-admins", "Production")

	args := []string{"threshold", "attest", "sign",
		"backup_manifest", "db1.full.x",
		"--repo", w.repoURL,
		"--hash", "deadbeef",
		"--roster", "prod-admins",
		"-o", "json"}
	if _, _, exit := runCLI(t, args...); exit != int(output.ExitOK) {
		t.Fatalf("first sign exit = %d", exit)
	}
	// Second sign:
	//   - header re-put is byte-identical → idempotent OK.
	//   - signature re-put is bit-different (timestamp drift) →
	//     ErrSubjectAlreadySigned → exit 7.  Matches the
	//     "no double-sign with different content" guard.
	_, errb, exit := runCLI(t, args...)
	if exit != int(output.ExitConflict) {
		t.Errorf("second sign exit = %d, want ExitConflict", exit)
	}
	if !strings.Contains(errb, "conflict.already_signed") {
		t.Errorf("expected conflict.already_signed:\n%s", errb)
	}
}

// plantForgedRoster writes a self-consistent roster signed by a key the
// local operator does NOT control — the repo-write attacker model. The
// CLI can't produce such a roster (roster create always signs with the
// local key), so we reach into the threshold package directly.
func plantForgedRoster(t *testing.T, w *readWorld, id string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	atk := bobSigner{pub: pub, priv: priv}
	r := threshold.NewRoster(id, "forged", 1,
		[]threshold.Member{threshold.NewMember("mallory@evil", pub)},
		time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	if err := threshold.SignRoster(r, atk, "mallory@evil"); err != nil {
		t.Fatal(err)
	}
	if err := threshold.NewRosterStore(w.sp).Put(context.Background(), r); err != nil {
		t.Fatalf("plant forged roster: %v", err)
	}
}

// TestThresholdAttestSign_ForgedRosterRefused proves the attest-sign read
// path is trust-anchored: signing against a roster the local operator
// didn't create is refused, so an operator can't be tricked into lending
// a signature to a planted roster.
func TestThresholdAttestSign_ForgedRosterRefused(t *testing.T) {
	w := newReadWorld(t)
	plantForgedRoster(t, w, "forged")
	_, errb, exit := runCLI(t, "threshold", "attest", "sign",
		"backup_manifest", "db1.full.x",
		"--repo", w.repoURL,
		"--hash", "deadbeef",
		"--roster", "forged",
		"-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("exit = %d, want ExitVerifyFailed", exit)
	}
	if !strings.Contains(errb, "verify.roster_untrusted") {
		t.Errorf("expected verify.roster_untrusted:\n%s", errb)
	}
}

// TestThresholdAttestVerify_ForgedRosterRefused proves the attest-verify
// read path is trust-anchored: a forged roster (even with a complete,
// self-consistent attestation under it) does not verify as satisfied.
func TestThresholdAttestVerify_ForgedRosterRefused(t *testing.T) {
	w := newReadWorld(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	atk := bobSigner{pub: pub, priv: priv}
	forged := threshold.NewRoster("forged", "forged", 1,
		[]threshold.Member{threshold.NewMember("mallory@evil", pub)},
		time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	if err := threshold.SignRoster(forged, atk, "mallory@evil"); err != nil {
		t.Fatal(err)
	}
	if err := threshold.NewRosterStore(w.sp).Put(context.Background(), forged); err != nil {
		t.Fatal(err)
	}
	// Attacker plants a complete quorum attestation under the forged roster.
	subject := threshold.AttestationSubject{Kind: "backup_manifest", ID: "db1.full.x", Hash: "deadbeef"}
	now := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)
	sig, err := threshold.SignAttestation(subject, forged, atk, "", now)
	if err != nil {
		t.Fatal(err)
	}
	as := threshold.NewAttestationStore(w.sp)
	if err := as.PutHeader(context.Background(), &threshold.AttestationHeader{
		Schema: threshold.SchemaAttestationHeader, Subject: subject,
		RosterID: forged.ID, RosterHash: threshold.RosterHash(forged),
		Threshold: forged.Threshold, CreatedAt: now.Truncate(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := as.PutSignature(context.Background(), sig); err != nil {
		t.Fatal(err)
	}

	_, errb, exit := runCLI(t, "threshold", "attest", "verify",
		"backup_manifest", "db1.full.x",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("exit = %d, want ExitVerifyFailed", exit)
	}
	if !strings.Contains(errb, "verify.roster_untrusted") {
		t.Errorf("expected verify.roster_untrusted:\n%s", errb)
	}
}

func TestThresholdAttestVerify_NotFound(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "threshold", "attest", "verify",
		"backup_manifest", "ghost",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound", exit)
	}
	if !strings.Contains(errb, "notfound.attestation") {
		t.Errorf("expected notfound.attestation:\n%s", errb)
	}
}

func TestThresholdAttestVerify_QuorumMet_KofN1(t *testing.T) {
	w := newReadWorld(t)
	mustCreateRosterLocal(t, w, "prod-admins", "Production")
	mustSignAttestation(t, w, "prod-admins", "backup_manifest", "db1.full.x", "deadbeef")

	stdout, _, exit := runCLI(t, "threshold", "attest", "verify",
		"backup_manifest", "db1.full.x",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d, want ExitOK\n%s", exit, stdout)
	}
	var v thresholdVerifyView
	bodyOf(t, stdout, &v)
	if !v.Met {
		t.Errorf("Met = false, want true")
	}
	if v.ValidSignatures != 1 {
		t.Errorf("ValidSignatures = %d, want 1", v.ValidSignatures)
	}
}

// TestThresholdAttestVerify_QuorumNotMet creates a 2-of-2 roster
// with the local key + an out-of-band member, signs only with the
// local key → 1-of-2 → exit 9 + body emitted (dual-stream).
func TestThresholdAttestVerify_QuorumNotMet(t *testing.T) {
	w := newReadWorld(t)
	_, pub := localFingerprint(t)
	memberSpec, _, _ := generateOutOfBandMember(t, "bob@acme.example")

	if _, _, exit := runCLI(t, "threshold", "roster", "create", "two-of-two",
		"--repo", w.repoURL,
		"--threshold", "2",
		"--member", "alice@acme.example:"+pub,
		"--member", memberSpec,
		"-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("create exit = %d", exit)
	}
	mustSignAttestation(t, w, "two-of-two", "backup_manifest", "db1.full.x", "deadbeef")

	stdout, errb, exit := runCLI(t, "threshold", "attest", "verify",
		"backup_manifest", "db1.full.x",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("exit = %d, want ExitVerifyFailed (9)\n%s", exit, errb)
	}
	if !strings.Contains(errb, "verify.quorum_not_met") {
		t.Errorf("expected verify.quorum_not_met on stderr:\n%s", errb)
	}
	// Dual-stream: body still emitted on stdout.
	var v thresholdVerifyView
	bodyOf(t, stdout, &v)
	if v.Met {
		t.Errorf("body Met = true, want false")
	}
	if v.ValidSignatures != 1 {
		t.Errorf("body ValidSignatures = %d, want 1", v.ValidSignatures)
	}
	if v.Threshold != 2 {
		t.Errorf("body Threshold = %d, want 2", v.Threshold)
	}
}

// TestThresholdAttestVerify_TwoSignersQuorumMet exercises the
// multi-operator path by manually injecting a second member's
// signature via the threshold package (CLI can only be invoked as
// the local operator).
func TestThresholdAttestVerify_TwoSignersQuorumMet(t *testing.T) {
	w := newReadWorld(t)
	_, localPub := localFingerprint(t)
	memberSpec, bobPub, bobPriv := generateOutOfBandMember(t, "bob@acme.example")

	// 2-of-2 roster with local + bob.
	if _, _, exit := runCLI(t, "threshold", "roster", "create", "two-met",
		"--repo", w.repoURL,
		"--threshold", "2",
		"--member", "alice@acme.example:"+localPub,
		"--member", memberSpec,
		"-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("create exit = %d", exit)
	}
	// Local operator signs as alice via CLI.
	mustSignAttestation(t, w, "two-met", "backup_manifest", "db1.full.x", "deadbeef")

	// Bob signs out-of-band by reaching into the threshold package
	// directly (simulates an admin operator running this on bob's
	// host with bob's keystore — same code path either way).
	rosterStore := threshold.NewRosterStore(w.sp)
	r, err := rosterStore.Get(context.Background(), "two-met")
	if err != nil {
		t.Fatal(err)
	}
	subject := threshold.AttestationSubject{
		Kind: "backup_manifest", ID: "db1.full.x", Hash: "deadbeef",
	}
	bobSig, err := threshold.SignAttestation(subject, r,
		bobSigner{pub: bobPub, priv: bobPriv}, "", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := threshold.NewAttestationStore(w.sp).PutSignature(
		context.Background(), bobSig); err != nil {
		t.Fatal(err)
	}

	// Now CLI verify should see 2-of-2.
	stdout, _, exit := runCLI(t, "threshold", "attest", "verify",
		"backup_manifest", "db1.full.x",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("verify exit = %d", exit)
	}
	var v thresholdVerifyView
	bodyOf(t, stdout, &v)
	if !v.Met {
		t.Errorf("Met = false, want true")
	}
	if v.ValidSignatures != 2 {
		t.Errorf("ValidSignatures = %d, want 2", v.ValidSignatures)
	}
}

func TestThresholdAttestShow_Happy(t *testing.T) {
	w := newReadWorld(t)
	mustCreateRosterLocal(t, w, "prod-admins", "Production")
	mustSignAttestation(t, w, "prod-admins", "backup_manifest", "db1.full.x", "deadbeef")

	stdout, _, exit := runCLI(t, "threshold", "attest", "show",
		"backup_manifest", "db1.full.x",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v thresholdShowView
	bodyOf(t, stdout, &v)
	if !v.RosterAvailable {
		t.Errorf("RosterAvailable = false")
	}
	if !v.QuorumMet {
		t.Errorf("QuorumMet = false")
	}
	if v.ValidDistinct != 1 {
		t.Errorf("ValidDistinct = %d, want 1", v.ValidDistinct)
	}
	if len(v.Signatures) != 1 || !v.Signatures[0].Valid {
		t.Errorf("Signatures shape: %+v", v.Signatures)
	}
}

// TestThreshold_HelpDiscoverable: parent help names every subcommand.
func TestThreshold_HelpDiscoverable(t *testing.T) {
	stdout, _, exit := runCLI(t, "threshold", "--help")
	if exit != int(output.ExitOK) {
		t.Fatalf("help exit = %d\n%s", exit, stdout)
	}
	for _, want := range []string{"whoami", "roster", "attest"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help missing %q:\n%s", want, stdout)
		}
	}
}

// ----- helpers -----

// bobSigner mirrors signerFromKey from threshold_test.go but lives
// here so the cli_test package needn't import the threshold-internal
// test helpers.
type bobSigner struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func (s bobSigner) Sign(payload []byte) []byte   { return ed25519.Sign(s.priv, payload) }
func (s bobSigner) PublicKey() ed25519.PublicKey { return s.pub }

// mustCreateRosterLocal creates a 1-of-1 roster with the local key
// as the only member.  The roster ID is the supplied name.
func mustCreateRosterLocal(t *testing.T, w *readWorld, id, description string) {
	t.Helper()
	_, pub := localFingerprint(t)
	args := []string{"threshold", "roster", "create", id,
		"--repo", w.repoURL,
		"--threshold", "1",
		"--member", "alice@acme.example:" + pub,
		"-o", "json"}
	if description != "" {
		args = append(args, "--description", description)
	}
	stdout, _, exit := runCLI(t, args...)
	if exit != int(output.ExitOK) {
		t.Fatalf("create %s: exit = %d\n%s", id, exit, stdout)
	}
}

// mustSignAttestation runs `threshold attest sign` and fails the test
// on a non-zero exit.
func mustSignAttestation(t *testing.T, w *readWorld, rosterID, kind, id, hash string) {
	t.Helper()
	stdout, _, exit := runCLI(t, "threshold", "attest", "sign",
		kind, id,
		"--repo", w.repoURL,
		"--hash", hash,
		"--roster", rosterID,
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("sign exit = %d\n%s", exit, stdout)
	}
}

// _ ensures encoding/json import lands even if no test directly uses it.
var _ = stdjson.Marshal
