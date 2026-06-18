// flow_restore.go — simple-CLI flow #6: restore a backup to an empty (or 'replace') target dir.
package simple

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/simple/prompt"
)

// flowRestore is operation #6 — bring a backup back to a directory
// on disk.
//
// Asks for deployment + backup ID + target dir, validates that the
// target is either empty or the operator typed "replace", runs the
// restore through internal/restore.Restore, then prints the next-
// step pg_ctl invocation.  Postverify is left at the package default
// (auto) — see the issue-#31 fix; soak coverage now wires trailing
// WAL into the repo, so postverify is the right gate for an
// operator who wants confidence the restored cluster actually
// starts.
type flowRestore struct{}

// Name implements Flow; returns the menu label printed before this
// operation runs.
func (flowRestore) Name() string { return "restore a backup" }

// Run implements Flow; picks a deployment + backup ID + target
// directory, requires "replace" confirmation when the target is
// non-empty, and drives internal/restore.Restore with the keyring's
// verifier and the local-KEK resolver.
func (f flowRestore) Run(ctx context.Context, env *Env) error {
	dep, err := pickDeployment(env, "Which database to restore?")
	if err != nil {
		if errors.Is(err, errNoDeployments) {
			env.Prompter.Println("  no deployments yet — pick #1 from the menu.")
			return nil
		}
		return err
	}

	verifier, err := loadVerifier(env)
	if err != nil {
		return fmt.Errorf("keystore: %w", err)
	}
	_, sp, err := repo.Open(ctx, dep.Repo)
	if err != nil {
		return fmt.Errorf("open repo: %w", err)
	}
	store := backup.NewManifestStore(sp)
	var manifests []*backup.Manifest
	for m, mErr := range store.List(ctx, dep.Name, verifier) {
		if mErr == nil {
			manifests = append(manifests, m)
		}
	}
	sp.Close()
	if len(manifests) == 0 {
		env.Prompter.Println("  no backups to restore.")
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

	defaultTarget := env.State.LastTargetDir
	if defaultTarget == "" {
		defaultTarget = filepath.Join(os.TempDir(), "pg_hardstorage-restored")
	}
	target, err := env.Prompter.PromptValid(
		"Restore into which directory? (will be created if missing)",
		defaultTarget,
		validateTargetDir,
	)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	allowOverwrite := false
	if entries, _ := os.ReadDir(target); len(entries) > 0 {
		env.Prompter.Printf("  %s is not empty.\n", target)
		ans, err := env.Prompter.PromptValid(
			`type "replace" to confirm overwriting it, or anything else to cancel`,
			"",
			func(s string) error { return nil },
		)
		if err != nil {
			return err
		}
		if ans != "replace" {
			env.Prompter.Println("  cancelled.")
			return nil
		}
		allowOverwrite = true
	}

	env.Prompter.Printf("\n  About to restore:\n")
	env.Prompter.Printf("    backup: %s (%d files)\n", m.BackupID, len(m.Files))
	env.Prompter.Printf("    target: %s\n\n", target)
	ok, err := env.Prompter.YesNo("Continue?", true)
	if err != nil {
		return err
	}
	if !ok {
		env.Prompter.Println("  cancelled.")
		return nil
	}

	env.Prompter.Println("  running restore...")
	start := time.Now()
	res, err := restore.Restore(ctx, restore.Options{
		RepoURL:        dep.Repo,
		Deployment:     dep.Name,
		BackupID:       m.BackupID,
		TargetDir:      target,
		Verifier:       verifier,
		AllowOverwrite: allowOverwrite,
		KEKForRef:      kekResolver(env),
	})
	if err != nil {
		return err
	}
	env.Prompter.Printf("\n  ✓ restored %d files (%d chunks · %d bytes) in %s\n\n",
		res.FileCount, res.ChunksFetched, res.BytesWritten, time.Since(start).Round(time.Second))
	env.Prompter.Printf("  to start the restored cluster:\n")
	env.Prompter.Printf("    pg_ctl -D %s start\n\n", target)

	env.State.LastDeployment = dep.Name
	env.State.LastTargetDir = target
	return nil
}

// validateTargetDir does the cheap pre-flight: absolute path, not
// equal to "/", parent dir must exist (or be creatable).  The
// emptiness check lives in Run because it depends on the operator's
// "replace" confirmation.
func validateTargetDir(s string) error {
	if s == "" {
		return errors.New("empty path")
	}
	if !filepath.IsAbs(s) {
		return errors.New("target must be an absolute path")
	}
	if s == "/" {
		return errors.New(`refusing to restore into "/"`)
	}
	return nil
}

// kekResolver returns the callback restore.Options.KEKForRef
// expects.  We only support the local-KEK ref today; cloud-KMS
// resolution is the full CLI's job (kept off the simple binary's
// surface).
func kekResolver(env *Env) func(ref string) ([encryption.KeyLen]byte, error) {
	return func(ref string) ([encryption.KeyLen]byte, error) {
		if env.Paths == nil || env.Paths.Keyring.Value == "" {
			return [encryption.KeyLen]byte{}, errors.New("no keyring path resolved")
		}
		if !keystore.KEKExists(env.Paths.Keyring.Value) {
			return [encryption.KeyLen]byte{}, fmt.Errorf("KEK %q referenced but no KEK on disk", ref)
		}
		kek, _, err := keystore.LoadOrGenerateKEK(env.Paths.Keyring.Value)
		return kek, err
	}
}
