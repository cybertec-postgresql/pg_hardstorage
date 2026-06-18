// flow_inspect.go — simple-CLI flow #4: read-only per-deployment repo summary.
package simple

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// flowInspect is operation #4: "See what's in my repository".
// Read-only — walks every deployment in the operator's config, opens
// the repo, lists manifests, prints a per-deployment summary table.
type flowInspect struct{}

// Name implements Flow; returns the menu label printed before this
// operation runs.
func (flowInspect) Name() string { return "inspect repo" }

// Run implements Flow; walks every configured deployment, lists
// the manifests in each repo, and prints a per-deployment summary
// table.  Read-only — no keyring or KEK is generated on this path.
func (flowInspect) Run(ctx context.Context, env *Env) error {
	deps := deploymentList(env)
	if len(deps) == 0 {
		env.Prompter.Println("  No deployments are configured.  Pick #1 to set one up.")
		return nil
	}

	verifier, _ := loadVerifier(env)

	for _, dep := range deps {
		env.Prompter.Printf("  %s   %s\n", dep.Name, dep.Repo)
		_, sp, err := repo.Open(ctx, dep.Repo)
		if err != nil {
			env.Prompter.Printf("    (repo unreachable: %v)\n\n", err)
			continue
		}
		store := backup.NewManifestStore(sp)
		var manifests []*backup.Manifest
		for m, mErr := range store.List(ctx, dep.Name, verifier) {
			if mErr != nil {
				env.Prompter.Printf("    (manifest read error: %v)\n", mErr)
				continue
			}
			manifests = append(manifests, m)
		}
		sp.Close()

		if len(manifests) == 0 {
			env.Prompter.Println("    no backups yet")
			env.Prompter.Println("")
			continue
		}
		// Newest first — operators read top-down.
		sort.Slice(manifests, func(i, j int) bool {
			return manifests[i].StartedAt.After(manifests[j].StartedAt)
		})

		env.Prompter.Printf("    %d backup(s)\n\n", len(manifests))
		env.Prompter.Printf("    %-40s  %-18s  %-8s  %s\n",
			"BACKUP ID", "WHEN", "FILES", "ENC")
		for _, m := range manifests {
			when := humanWhen(m.StartedAt)
			enc := "—"
			if m.Encryption != nil {
				enc = m.Encryption.KEKRef
			}
			env.Prompter.Printf("    %-40s  %-18s  %-8d  %s\n",
				m.BackupID, when, len(m.Files), enc)
		}
		env.Prompter.Println("")
	}
	return nil
}

// humanWhen renders a time.Time as a "2 hours ago" / "yesterday"
// string for the inspect / verify / restore tables.  Clamp
// resolution at "minutes" for very recent rows so we never get
// jittery "5 seconds ago" stamps.
func humanWhen(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < 2*time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.UTC().Format("2006-01-02")
	}
}

// loadVerifier returns a Verifier built from the operator's keyring.
// On first run (no keypair on disk yet) it returns (nil, nil) so the
// inspect flow can still render an empty-repo view; manifests read
// with a nil verifier surface as "manifest read error" lines rather
// than aborting the whole flow.
//
// We deliberately don't call keystore.LoadOrGenerate from the read-
// only inspect path — that would create a fresh keypair on disk just
// because the operator picked menu item #4.
func loadVerifier(env *Env) (*backup.Verifier, error) {
	if env.Paths == nil || env.Paths.Keyring.Value == "" {
		return nil, nil
	}
	priv := filepath.Join(env.Paths.Keyring.Value, keystore.PrivateKeyFile)
	pub := filepath.Join(env.Paths.Keyring.Value, keystore.PublicKeyFile)
	if _, err := os.Stat(priv); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if _, err := os.Stat(pub); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	_, verifier, err := keystore.LoadOrGenerate(env.Paths.Keyring.Value)
	if err != nil {
		return nil, err
	}
	return verifier, nil
}
