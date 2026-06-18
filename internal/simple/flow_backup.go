// flow_backup.go — simple-CLI flow #2: take one full backup right now (no flags).
package simple

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

// flowBackup is operation #2 — take one full backup, right now.
//
// Picks (or auto-selects) a deployment, runs the same runner.Take
// the full CLI's `backup` command uses, renders the result as a
// short prose line.  No flags exposed; encryption is on if the
// keyring has a KEK file (mirrors the `init` wizard's default).
type flowBackup struct{}

// Name implements Flow; returns the menu label printed before this
// operation runs.
func (flowBackup) Name() string { return "take a backup" }

// Run implements Flow; picks a deployment, confirms the operation,
// and drives runner.Take with the keyring's signer (and KEK if one
// exists on disk).
func (f flowBackup) Run(ctx context.Context, env *Env) error {
	dep, err := pickDeployment(env, "Which database do you want to back up?")
	if err != nil {
		if errors.Is(err, errNoDeployments) {
			env.Prompter.Println("  no deployments yet — pick #1 from the menu to set one up.")
			return nil
		}
		return err
	}
	return f.runFor(ctx, env, dep)
}

// runFor is the shared body called both from this flow's Run and
// from flowSetup's "take a first backup right now?" affordance.
// Keeps the prompts + commit logic in one place.
func (f flowBackup) runFor(ctx context.Context, env *Env, dep Deployment) error {
	env.Prompter.Printf("\n  About to take a full backup of %s:\n", dep.Name)
	env.Prompter.Printf("    source: %s\n", redactPassword(dep.PGConnection))
	env.Prompter.Printf("    target: %s\n\n", dep.Repo)
	ok, err := env.Prompter.YesNo("Continue?", true)
	if err != nil {
		return err
	}
	if !ok {
		env.Prompter.Println("  cancelled.")
		return nil
	}

	signer, _, err := keystore.LoadOrGenerate(env.Paths.Keyring.Value)
	if err != nil {
		return fmt.Errorf("keystore: %w", err)
	}

	opts := runner.TakeOptions{
		PGConnString: dep.PGConnection,
		RepoURL:      dep.Repo,
		Deployment:   dep.Name,
		Tenant:       dep.Tenant,
		Signer:       signer,
	}
	// Encryption is on if a KEK is on disk (mirrors the full
	// `init` wizard's default).  We deliberately don't auto-
	// generate one from this flow — picking #2 without first
	// having run #1 means "no encryption", not "make one now
	// silently".
	if kek, kekErr := loadKEKIfPresent(env); kekErr != nil {
		return kekErr
	} else if kek != nil {
		opts.Encryption = &runner.EncryptionConfig{KEK: *kek, KEKRef: keystore.KEKRefLocal}
	}

	env.Prompter.Println("  running backup...")
	start := time.Now()
	res, err := runner.Take(ctx, opts)
	if err != nil {
		return err
	}
	env.Prompter.Printf("\n  ✓ %s\n", res.BackupID)
	env.Prompter.Printf("    %d files · %d unique chunks · %s\n\n",
		res.FileCount, res.UniqueChunkCount, time.Since(start).Round(time.Second))

	// Surface the "no WAL stream running" hint if it applies.  We
	// can't detect a sidecar streamer from this process; rely on
	// the operator's awareness from previous menu choices via
	// State.  (A real cross-process check is more invasive than
	// the hint warrants.)
	env.Prompter.Println("  this backup is restorable to its stop point.")
	env.Prompter.Println("  for point-in-time recovery between backups, pick #3 to stream WAL.\n")

	env.State.LastDeployment = dep.Name
	return nil
}

// loadKEKIfPresent returns the KEK bytes from disk if the operator
// generated one (during a prior #1 setup); returns (nil, nil) if no
// KEK file exists — that's the unencrypted-on-purpose path.  We
// guard with KEKExists first so this never *generates* a fresh KEK
// from the backup path (only #1 does that, on explicit consent).
func loadKEKIfPresent(env *Env) (*[encryption.KeyLen]byte, error) {
	if env.Paths == nil || env.Paths.Keyring.Value == "" {
		return nil, nil
	}
	if !keystore.KEKExists(env.Paths.Keyring.Value) {
		return nil, nil
	}
	kek, _, err := keystore.LoadOrGenerateKEK(env.Paths.Keyring.Value)
	if err != nil {
		return nil, err
	}
	return &kek, nil
}
