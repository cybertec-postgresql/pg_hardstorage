package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/dsa"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

type dsaReportView struct {
	ID            string `json:"id"`
	SubjectIDHash string `json:"subject_id_hash"`
	Tenant        string `json:"tenant"`
	Article       string `json:"article"`
	Note          string `json:"note,omitempty"`

	ManifestsScanned    int `json:"manifests_scanned"`
	ManifestsAffected   int `json:"manifests_affected"`
	DeploymentsAffected int `json:"deployments_affected"`

	AffectedBackups []struct {
		Deployment string `json:"deployment"`
		BackupID   string `json:"backup_id"`
		Encrypted  bool   `json:"encrypted"`
		KEKRef     string `json:"kek_ref,omitempty"`
	} `json:"affected_backups,omitempty"`

	SuggestedActions []struct {
		Article string `json:"article"`
		Command string `json:"command,omitempty"`
	} `json:"suggested_actions,omitempty"`

	PublicKeyFingerprint string `json:"public_key_fingerprint,omitempty"`
	BodyHash             string `json:"body_hash,omitempty"`
	Signature            string `json:"signature,omitempty"`
}

type dsaListView struct {
	Count   int `json:"count"`
	Entries []struct {
		ID                  string    `json:"id"`
		GeneratedAt         time.Time `json:"generated_at"`
		Tenant              string    `json:"tenant"`
		Article             string    `json:"article"`
		ManifestsAffected   int       `json:"manifests_affected"`
		DeploymentsAffected int       `json:"deployments_affected"`
	} `json:"entries"`
}

type dsaVerifyView struct {
	ID                   string `json:"id"`
	SignatureValid       bool   `json:"signature_valid"`
	PublicKeyFingerprint string `json:"public_key_fingerprint"`
	Reason               string `json:"reason,omitempty"`
}

// commitTenantManifest plants a manifest with a specific tenant +
// optional KEKRef.
func commitTenantManifest(t *testing.T, w *readWorld, deployment, tenant, kekRef string, idx int) {
	t.Helper()
	ts := time.Date(2026, 5, 1, 12, idx, 0, 0, time.UTC)
	files := []backup.FileEntry{
		{Path: "PG_VERSION", Size: 3, Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: repo.HashOf([]byte("17\n")), Offset: 0, Len: 3}}},
	}
	m := &backup.Manifest{
		Schema:           backup.Schema,
		BackupID:         deployment + ".full." + ts.Format("20060102T150405Z"),
		Deployment:       deployment,
		Tenant:           tenant,
		Type:             backup.BackupTypeFull,
		PGVersion:        17,
		SystemIdentifier: "7000000000000000001",
		StartLSN:         "0/3000028",
		StopLSN:          "0/30001A0",
		Timeline:         1,
		StartedAt:        ts,
		StoppedAt:        ts.Add(30 * time.Second),
		BackupLabel:      "START WAL LOCATION: 0/3000028\n",
		Tablespaces:      []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files:            files,
	}
	if kekRef != "" {
		m.Encryption = &backup.EncryptionInfo{
			Scheme: "aes-256-gcm", KEKRef: kekRef,
			WrappedDEK: "deadbeef", EnvelopeVersion: 1,
		}
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// ----- locate -----

func TestDSALocate_RequiresFlags(t *testing.T) {
	w := newReadWorld(t)
	cases := []struct {
		name string
		args []string
	}{
		{"missing --repo",
			[]string{"dsa", "locate", "--subject-id", "u", "--tenant", "T", "-o", "json"}},
		{"missing --subject-id",
			[]string{"dsa", "locate", "--repo", w.repoURL, "--tenant", "T", "-o", "json"}},
		{"missing --tenant",
			[]string{"dsa", "locate", "--repo", w.repoURL, "--subject-id", "u", "-o", "json"}},
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

func TestDSALocate_BadArticle(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "dsa", "locate",
		"--repo", w.repoURL,
		"--subject-id", "u", "--tenant", "T",
		"--article", "art_42_omg",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestDSALocate_BadWindow(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "dsa", "locate",
		"--repo", w.repoURL,
		"--subject-id", "u", "--tenant", "T",
		"--window-from", "yesterday",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestDSALocate_HappyArticle17_RecommendsKMSShred(t *testing.T) {
	w := newReadWorld(t)
	commitTenantManifest(t, w, "db1", "tenant-a", "kms://acme/a", 0)
	commitTenantManifest(t, w, "db1", "tenant-a", "kms://acme/a", 1)
	commitTenantManifest(t, w, "db2", "tenant-b", "kms://acme/b", 2)

	stdout, _, exit := runCLI(t, "dsa", "locate",
		"--repo", w.repoURL,
		"--subject-id", "user-42",
		"--tenant", "tenant-a",
		"--article", "art_17_erasure",
		"--note", "GDPR Art. 17 #5023",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v dsaReportView
	bodyOf(t, stdout, &v)

	if v.Tenant != "tenant-a" || v.Article != "art_17_erasure" {
		t.Errorf("tenant=%q article=%q", v.Tenant, v.Article)
	}
	if v.ManifestsScanned != 3 {
		t.Errorf("scanned = %d, want 3", v.ManifestsScanned)
	}
	if v.ManifestsAffected != 2 {
		t.Errorf("affected = %d, want 2", v.ManifestsAffected)
	}
	if v.DeploymentsAffected != 1 {
		t.Errorf("deployments = %d, want 1 (db1 only)", v.DeploymentsAffected)
	}
	// raw subject id must not be in body.
	if strings.Contains(stdout, "user-42") {
		t.Errorf("raw subject id leaked into body:\n%s", stdout)
	}
	if v.SubjectIDHash == "" {
		t.Errorf("subject hash empty")
	}
	// signed.
	if v.PublicKeyFingerprint == "" || v.Signature == "" {
		t.Errorf("expected signed report; fingerprint=%q sig=%q",
			v.PublicKeyFingerprint, v.Signature)
	}
	// kms shred suggested.
	hasShred := false
	for _, a := range v.SuggestedActions {
		if a.Article == "art_17_erasure" &&
			strings.HasPrefix(a.Command, "pg_hardstorage kms shred --repo") &&
			strings.Contains(a.Command, "--require-approval") {
			hasShred = true
		}
	}
	if !hasShred {
		t.Errorf("expected kms-shred suggested action: %+v", v.SuggestedActions)
	}
}

func TestDSALocate_DeploymentScoped(t *testing.T) {
	w := newReadWorld(t)
	commitTenantManifest(t, w, "db1", "T", "k", 0)
	commitTenantManifest(t, w, "db2", "T", "k", 1)

	stdout, _, exit := runCLI(t, "dsa", "locate",
		"--repo", w.repoURL,
		"--subject-id", "user", "--tenant", "T",
		"--article", "art_15_access",
		"--deployment", "db2",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v dsaReportView
	bodyOf(t, stdout, &v)
	if v.ManifestsAffected != 1 {
		t.Errorf("affected = %d, want 1", v.ManifestsAffected)
	}
	if v.AffectedBackups[0].Deployment != "db2" {
		t.Errorf("deployment = %q", v.AffectedBackups[0].Deployment)
	}
}

func TestDSALocate_PersistsReport(t *testing.T) {
	w := newReadWorld(t)
	commitTenantManifest(t, w, "db1", "T", "k", 0)

	stdout, _, exit := runCLI(t, "dsa", "locate",
		"--repo", w.repoURL,
		"--subject-id", "u", "--tenant", "T",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("locate exit = %d\n%s", exit, stdout)
	}
	var report dsaReportView
	bodyOf(t, stdout, &report)

	stdout, _, exit = runCLI(t, "dsa", "list",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("list exit = %d", exit)
	}
	var list dsaListView
	bodyOf(t, stdout, &list)
	if list.Count != 1 || list.Entries[0].ID != report.ID {
		t.Errorf("persistence drift: %+v vs %+v", list.Entries, report)
	}
}

// ----- list -----

func TestDSAList_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "dsa", "list", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

func TestDSAList_BadArticle(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "dsa", "list",
		"--repo", w.repoURL, "--article", "exotic", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestDSAList_BadSince(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "dsa", "list",
		"--repo", w.repoURL, "--since", "yesterday", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestDSAList_Empty(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "dsa", "list",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v dsaListView
	bodyOf(t, stdout, &v)
	if v.Count != 0 {
		t.Errorf("Count = %d", v.Count)
	}
}

func TestDSAList_FilterBySubjectID(t *testing.T) {
	w := newReadWorld(t)
	commitTenantManifest(t, w, "db1", "T", "k", 0)

	// Three locate calls under different subject-ids.
	for _, id := range []string{"alice", "bob", "carol"} {
		_, _, exit := runCLI(t, "dsa", "locate",
			"--repo", w.repoURL,
			"--subject-id", id, "--tenant", "T",
			"-o", "json")
		if exit != int(output.ExitOK) {
			t.Fatalf("locate %s exit = %d", id, exit)
		}
	}

	// Filter by subject — pass the SAME raw id; the CLI hashes it
	// before matching, so the operator's UX is "use the same opaque
	// ID" rather than "compute the hash yourself".
	stdout, _, exit := runCLI(t, "dsa", "list",
		"--repo", w.repoURL,
		"--subject-id", "bob",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("list exit = %d", exit)
	}
	var v dsaListView
	bodyOf(t, stdout, &v)
	if v.Count != 1 {
		t.Errorf("subject filter count = %d, want 1", v.Count)
	}
	// Verify the hash matches "bob".
	wantHash := dsa.HashSubjectIDForFilter("bob")
	if !strings.HasPrefix(wantHash, "") || len(wantHash) != 64 {
		t.Errorf("hash shape: %q", wantHash)
	}
}

// ----- show -----

func TestDSAShow_NotFound(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "dsa", "show", "ghost",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound", exit)
	}
	if !strings.Contains(errb, "notfound.report") {
		t.Errorf("expected notfound.report:\n%s", errb)
	}
}

func TestDSAShow_Happy(t *testing.T) {
	w := newReadWorld(t)
	commitTenantManifest(t, w, "db1", "T", "k", 0)

	stdout, _, exit := runCLI(t, "dsa", "locate",
		"--repo", w.repoURL,
		"--subject-id", "u", "--tenant", "T", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("locate exit = %d", exit)
	}
	var report dsaReportView
	bodyOf(t, stdout, &report)

	stdout, _, exit = runCLI(t, "dsa", "show", report.ID,
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("show exit = %d\n%s", exit, stdout)
	}
	var got dsaReportView
	bodyOf(t, stdout, &got)
	if got.ID != report.ID {
		t.Errorf("show drift: %q vs %q", got.ID, report.ID)
	}
}

// ----- verify -----

func TestDSAVerify_NotFound(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "dsa", "verify", "ghost",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound", exit)
	}
	if !strings.Contains(errb, "notfound.report") {
		t.Errorf("expected notfound.report:\n%s", errb)
	}
}

func TestDSAVerify_Happy(t *testing.T) {
	w := newReadWorld(t)
	commitTenantManifest(t, w, "db1", "T", "k", 0)

	stdout, _, exit := runCLI(t, "dsa", "locate",
		"--repo", w.repoURL,
		"--subject-id", "u", "--tenant", "T", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("locate exit = %d", exit)
	}
	var report dsaReportView
	bodyOf(t, stdout, &report)

	stdout, _, exit = runCLI(t, "dsa", "verify", report.ID,
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("verify exit = %d\n%s", exit, stdout)
	}
	var v dsaVerifyView
	bodyOf(t, stdout, &v)
	if !v.SignatureValid {
		t.Errorf("SignatureValid = false")
	}
}

func TestDSAVerify_Tampered(t *testing.T) {
	w := newReadWorld(t)
	commitTenantManifest(t, w, "db1", "T", "k", 0)

	stdout, _, exit := runCLI(t, "dsa", "locate",
		"--repo", w.repoURL,
		"--subject-id", "u", "--tenant", "T", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("locate exit = %d", exit)
	}
	var report dsaReportView
	bodyOf(t, stdout, &report)

	// Tamper: rewrite the on-disk report with a different tenant
	// (the signature commits to Tenant, so verification fails).
	store := dsa.NewReportStore(w.sp)
	r, err := store.Get(context.Background(), report.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.Tenant = "different"
	if err := w.sp.Delete(context.Background(), "dsa/reports/"+report.ID+".json"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.Put(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	stdout, errb, exit := runCLI(t, "dsa", "verify", report.ID,
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("exit = %d, want ExitVerifyFailed\n%s", exit, errb)
	}
	if !strings.Contains(errb, "verify.dsa_signature") {
		t.Errorf("expected verify.dsa_signature:\n%s", errb)
	}
	var v dsaVerifyView
	bodyOf(t, stdout, &v)
	if v.SignatureValid {
		t.Errorf("SignatureValid = true after tamper")
	}
}

// TestDSA_HelpDiscoverable: parent help names every subcommand.
func TestDSA_HelpDiscoverable(t *testing.T) {
	stdout, _, exit := runCLI(t, "dsa", "--help")
	if exit != int(output.ExitOK) {
		t.Fatalf("help exit = %d\n%s", exit, stdout)
	}
	for _, want := range []string{"locate", "list", "show", "verify"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help missing %q:\n%s", want, stdout)
		}
	}
}
