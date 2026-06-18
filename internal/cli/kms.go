// kms.go — CLI surface for inspecting the on-disk keyring.
package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// newKmsCmd implements `pg_hardstorage kms` — the operator-facing
// surface of the keyring. v0.1.1 ships only the read-only `inspect`
// verb; the mutating verbs (rotate, shred, hsm-status) require
// substantial new infrastructure (rewrap walker; PKCS#11 binding)
// and remain stubs in the SPEC's+ tree.
//
// `inspect` answers the operator's "what's in my keyring?" question
// without ever READING the key bytes — it reports presence, file
// mode, and a SHA-256 fingerprint of the PUBLIC key (for the signing
// keypair only; the KEK's bytes never enter the inspection path).
// This is the same posture the doctor command takes for the
// keystore section: presence + metadata, never key material.
func newKmsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "kms",
		Short: "Manage encryption keys",
		Long: `v0.1.1 ships the read-only ` + "`kms inspect`" + ` surface. Mutating
verbs (rotate, shred, hsm-status) land alongside the KMS
plugin tier (gcp-kms, azure-key-vault, vault-transit) and the PKCS#11
/ TPM 2.0 integrations.`,
	}
	c.AddCommand(newKmsInspectCmd())
	c.AddCommand(newKMSRotateCmd())
	c.AddCommand(newKmsShredCmd())
	c.AddCommand(newKmsVerifyCmd())
	c.AddCommand(stub("hsm-status", "Report PKCS#11 / TPM 2.0 binding state (deferred)", ""))
	return c
}

func newKmsInspectCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "inspect",
		Short: "Read-only summary of the keyring",
		Long: `Reports presence, file mode, and (for the signing public key)
fingerprint. Private key bytes and KEK bytes are NEVER read or
displayed — operators paranoid about a future inspect-leak can
verify by reading the source.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runKmsInspect(cmd)
		},
	}
	return c
}

func runKmsInspect(cmd *cobra.Command) error {
	d := DispatcherFrom(cmd)
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return output.NewError("internal", err.Error()).Wrap(err)
	}

	body := kmsInspectBody{KeyringDir: p.Keyring.Value}
	body.SigningKey = inspectFile(p.Keyring.Value, keystore.PrivateKeyFile, false)
	body.SigningPub = inspectFile(p.Keyring.Value, keystore.PublicKeyFile, true)
	body.KEK = inspectFile(p.Keyring.Value, keystore.KEKFileName, false)

	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// inspectFile reports presence + mode (+ optionally a fingerprint
// of the public-key bytes). Reading public-key bytes is fine —
// they're public by definition. Reading the signing-private bytes
// or the KEK bytes is REFUSED here as a defensive posture (set
// readPubBytes=true only for the public-key path).
func inspectFile(dir, name string, readPubBytes bool) keyringFileReport {
	full := filepath.Join(dir, name)
	r := keyringFileReport{Name: name}
	info, err := os.Stat(full)
	if err != nil {
		// ErrNotExist is the common case ("no key here yet"); other
		// errors get reported so operators see the problem.
		if !os.IsNotExist(err) {
			r.StatError = err.Error()
		}
		return r
	}
	r.Present = true
	r.Mode = info.Mode().String()
	r.SizeBytes = info.Size()
	r.ModTime = info.ModTime().UTC().Format("2006-01-02T15:04:05Z")

	// Mode safety hint — the signing private key and the KEK MUST
	// be 0600 (owner-only). 0644 / 0640 is a finding worth surfacing
	// at inspect time, not just at first key-load.
	if info.Mode().Perm()&0o077 != 0 && !readPubBytes {
		// Anything more permissive than 0600 on a private key file
		// fails the safety bar.
		r.Warning = fmt.Sprintf("mode %s is too permissive for a private key (must be 0600)",
			info.Mode().Perm())
	}

	if readPubBytes {
		// Public-key fingerprint is genuinely useful — operators
		// audit "this fleet's manifests are signed by the same
		// keypair" by comparing fingerprints. SHA-256 of the raw
		// PEM bytes; first 16 hex chars is the operator-readable form.
		body, err := os.ReadFile(full)
		if err == nil {
			sum := sha256.Sum256(body)
			r.FingerprintSHA256 = hex.EncodeToString(sum[:8])
		}
	}
	return r
}

// Result body shapes — stable per the v1 schema commitment.

type keyringFileReport struct {
	Name              string `json:"name"`
	Present           bool   `json:"present"`
	Mode              string `json:"mode,omitempty"`
	SizeBytes         int64  `json:"size_bytes,omitempty"`
	ModTime           string `json:"mod_time,omitempty"`
	FingerprintSHA256 string `json:"fingerprint_sha256,omitempty"`
	Warning           string `json:"warning,omitempty"`
	StatError         string `json:"stat_error,omitempty"`
}

type kmsInspectBody struct {
	KeyringDir string            `json:"keyring_dir"`
	SigningKey keyringFileReport `json:"signing_key"`
	SigningPub keyringFileReport `json:"signing_pub"`
	KEK        keyringFileReport `json:"kek"`
}

// WriteText renders the keyring inspection — file presence, modes, and
// fingerprints — as human-readable text to w.
func (b kmsInspectBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "keyring at %s\n", b.KeyringDir)
	for _, e := range []keyringFileReport{b.SigningKey, b.SigningPub, b.KEK} {
		writeKeyringEntry(bw, e)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

func writeKeyringEntry(bw *strings.Builder, e keyringFileReport) {
	fmt.Fprintf(bw, "  %s\n", e.Name)
	if !e.Present {
		if e.StatError != "" {
			fmt.Fprintf(bw, "    stat error: %s\n", e.StatError)
		} else {
			fmt.Fprintf(bw, "    not present\n")
		}
		return
	}
	fmt.Fprintf(bw, "    present:     yes (mode %s, %d bytes, %s)\n",
		e.Mode, e.SizeBytes, e.ModTime)
	if e.FingerprintSHA256 != "" {
		fmt.Fprintf(bw, "    fingerprint: sha256:%s\n", e.FingerprintSHA256)
	}
	if e.Warning != "" {
		fmt.Fprintf(bw, "    ✗ %s\n", e.Warning)
	}
}
