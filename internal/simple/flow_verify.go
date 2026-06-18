// flow_verify.go — simple-CLI flow #5: per-chunk verify of a chosen manifest via the CAS read path.
package simple

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/simple/prompt"
)

// flowVerify is operation #5 — "verify a backup is restorable".
//
// Walks every chunk referenced by the chosen manifest through the
// CAS read path (hash check + decrypt-if-applicable) and reports
// pass/fail.  Same data plane the full CLI's `verify` command uses;
// rendering is collapsed to a single "X of Y chunks ok" line per
// pass.
type flowVerify struct{}

// Name implements Flow; returns the menu label printed before this
// operation runs.
func (flowVerify) Name() string { return "verify a backup" }

// Run implements Flow; picks a deployment + backup, walks every
// chunk referenced by the manifest through the CAS read path
// (hash check + decrypt-if-applicable), and prints an
// "ok / total" summary plus the first failing chunk if any.
func (f flowVerify) Run(ctx context.Context, env *Env) error {
	dep, err := pickDeployment(env, "Which database?")
	if err != nil {
		if errors.Is(err, errNoDeployments) {
			env.Prompter.Println("  no deployments yet — pick #1 from the menu.")
			return nil
		}
		return err
	}

	verifier, _ := loadVerifier(env)
	_, sp, err := repo.Open(ctx, dep.Repo)
	if err != nil {
		return fmt.Errorf("open repo: %w", err)
	}
	defer sp.Close()
	store := backup.NewManifestStore(sp)

	// Build the manifest picker.
	var manifests []*backup.Manifest
	for m, mErr := range store.List(ctx, dep.Name, verifier) {
		if mErr != nil {
			continue
		}
		manifests = append(manifests, m)
	}
	if len(manifests) == 0 {
		env.Prompter.Println("  no backups to verify yet.")
		return nil
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].StartedAt.After(manifests[j].StartedAt) })

	choices := make([]prompt.Choice, len(manifests))
	for i, m := range manifests {
		choices[i] = prompt.Choice{
			Label:  m.BackupID,
			Detail: fmt.Sprintf("%s · %d files", humanWhen(m.StartedAt), len(m.Files)),
		}
	}
	idx, err := env.Prompter.PromptChoice("Which backup?", choices, 0)
	if err != nil {
		return err
	}
	m := manifests[idx]

	// Build the CAS the chunk walk uses.  Mirrors internal/cli's
	// buildVerifyCAS but without the cobra plumbing: read the
	// wrapped DEK from the manifest, unwrap with the local KEK,
	// hand it to aesgcm.New, hand that to repo.NewCAS via
	// casdefault.NewEncrypted.
	cas, err := f.buildCAS(env, sp, m)
	if err != nil {
		return err
	}
	env.Prompter.Printf("\n  verifying %s ...\n", m.BackupID)
	var ok int
	var total int
	var firstFail string
	for _, file := range m.Files {
		for _, ref := range file.Chunks {
			total++
			if _, err := cas.GetChunkBytes(ctx, ref.Hash); err != nil {
				if firstFail == "" {
					firstFail = fmt.Sprintf("%s: %v", ref.Hash, err)
				}
			} else {
				ok++
			}
		}
	}
	if ok == total {
		env.Prompter.Printf("\n  ✓ %d / %d chunks ok\n\n", ok, total)
	} else {
		env.Prompter.Printf("\n  ✗ %d / %d chunks ok\n", ok, total)
		env.Prompter.Printf("    first failure: %s\n", firstFail)
		env.Prompter.Printf("    next step:  pg_hardstorage repair scrub --repo %s\n\n", dep.Repo)
	}
	env.State.LastDeployment = dep.Name
	return nil
}

// buildCAS wraps the storage plugin with the right encryptor when
// the manifest has an Encryption block.  Unencrypted manifests get
// the plain casdefault.New; we never error on "no encryption" — a
// mixed repo (some backups encrypted, some not) is supported.
func (flowVerify) buildCAS(env *Env, sp storage.StoragePlugin, m *backup.Manifest) (*repo.CAS, error) {
	if m.Encryption == nil {
		return casdefault.New(sp), nil
	}
	if !keystore.KEKExists(env.Paths.Keyring.Value) {
		return nil, fmt.Errorf("manifest is encrypted but no KEK on disk at %s",
			env.Paths.Keyring.Value)
	}
	kek, _, err := keystore.LoadOrGenerateKEK(env.Paths.Keyring.Value)
	if err != nil {
		return nil, fmt.Errorf("load KEK: %w", err)
	}
	wrapped, err := base64.StdEncoding.DecodeString(m.Encryption.WrappedDEK)
	if err != nil {
		return nil, fmt.Errorf("decode wrapped DEK: %w", err)
	}
	dek, err := encryption.Unwrap(kek, wrapped)
	if err != nil {
		return nil, fmt.Errorf("unwrap DEK (wrong KEK?): %w", err)
	}
	enc, err := aesgcm.New(dek[:])
	if err != nil {
		return nil, err
	}
	return casdefault.NewEncrypted(sp, enc), nil
}
