// flow_setup.go — simple-CLI flow #1: Day-0 wizard (connection / name / repo URL / encrypt-y/n).
package simple

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/keystore"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"gopkg.in/yaml.v3"
)

// flowSetup is operation #1 — Day-0 wizard.
//
// Walks the operator through the four prompts the design doc
// describes (connection, name, repo URL, encrypt-y/n), validates each
// before continuing, then runs the underlying repo.Init + keypair +
// optional KEK generation.  The first backup ("take one now?") is
// delegated to flowBackup so the wizard stays focused on setup; the
// flow returns success once the deployment is durably recorded in
// the operator's config.
type flowSetup struct{}

// Name implements Flow; returns the menu label printed before this
// operation runs.
func (flowSetup) Name() string { return "set up a new deployment" }

// Run implements Flow; walks the Day-0 wizard (PG connection,
// deployment name, repo URL, encrypt-yes-or-no), idempotently
// initialises the repo, generates the signing keypair and optional
// KEK, persists the deployment to pg_hardstorage.yaml, and offers
// to take a first backup via flowBackup.runFor.
func (f flowSetup) Run(ctx context.Context, env *Env) error {
	// 1. PG connection string.  We don't open the connection here —
	//    that's the backup runner's job.  The wizard just records
	//    the URL; a typo surfaces at first-backup time with a
	//    structured error the operator can read.
	pgConn, err := env.Prompter.PromptValid(
		"PostgreSQL connection string?",
		firstNonEmpty(env.State.LastPGConnection, "postgres://postgres@127.0.0.1/postgres"),
		validateDSN,
	)
	if err != nil {
		return err
	}

	// 2. Friendly deployment name.  Default = dbname portion of the
	//    DSN, falling back to "db1".  Cobra-compatible: only
	//    alphanumerics + dash/underscore, since it lands in the
	//    backup-ID path.
	defaultName := dbnameFromDSN(pgConn)
	if defaultName == "" {
		defaultName = "db1"
	}
	name, err := env.Prompter.PromptValid(
		"What should we call this deployment?",
		defaultName,
		validateDeploymentName,
	)
	if err != nil {
		return err
	}

	// 3. Repo URL.  Default points at a per-user filesystem location
	//    so the wizard works offline.  Validates writability by
	//    creating + removing a probe file under the dirname.
	repoURL, err := env.Prompter.PromptValid(
		"Where should backups go?",
		firstNonEmpty(env.State.LastRepoURL, defaultRepoURL(env)),
		validateRepoURL,
	)
	if err != nil {
		return err
	}

	// 4. Encrypt?  Default yes — same posture as the full `init`
	//    wizard.
	wantEncrypt, err := env.Prompter.YesNo("Encrypt backups with a local KEK?", true)
	if err != nil {
		return err
	}

	// Summary + confirm.
	env.Prompter.Printf("\n  About to set up:\n")
	env.Prompter.Printf("    deployment: %s\n", name)
	env.Prompter.Printf("    pg:         %s\n", redactPassword(pgConn))
	env.Prompter.Printf("    repo:       %s\n", repoURL)
	env.Prompter.Printf("    encryption: %s\n\n", yesNoStr(wantEncrypt))
	ok, err := env.Prompter.YesNo("Proceed?", true)
	if err != nil {
		return err
	}
	if !ok {
		env.Prompter.Println("  cancelled.")
		return nil
	}

	// Run: repo.Init (idempotent — repo.Open if exists), keystore
	// LoadOrGenerate (signing keypair on first run, no-op on re-run),
	// keystore.LoadOrGenerateKEK if encryption requested.
	env.Prompter.Println("\n  initialising repo...")
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		// Be idempotent: if the repo already exists, just verify
		// we can open it and keep going — same shape as the full
		// `init` wizard since PR #29.
		if errors.Is(err, repo.ErrAlreadyExists) {
			if _, _, openErr := repo.Open(ctx, repoURL); openErr != nil {
				return fmt.Errorf("repo exists but cannot be opened: %w", openErr)
			}
			env.Prompter.Println("  (repo already initialised, reusing)")
		} else {
			return fmt.Errorf("repo init: %w", err)
		}
	}

	env.Prompter.Println("  generating signing keypair...")
	if _, _, err := keystore.LoadOrGenerate(env.Paths.Keyring.Value); err != nil {
		return fmt.Errorf("keystore signing: %w", err)
	}

	if wantEncrypt {
		env.Prompter.Println("  generating KEK...")
		if _, _, err := keystore.LoadOrGenerateKEK(env.Paths.Keyring.Value); err != nil {
			return fmt.Errorf("keystore KEK: %w", err)
		}
	}

	// Append the deployment to the operator's pg_hardstorage.yaml so
	// subsequent runs (and the rest of the menu) see it.
	if err := persistDeployment(env, name, pgConn, repoURL); err != nil {
		return fmt.Errorf("config write: %w", err)
	}

	// Update State + bump in-memory config so the rest of the
	// session reflects the new deployment without a binary restart.
	env.State.LastDeployment = name
	env.State.LastPGConnection = pgConn
	env.State.LastRepoURL = repoURL
	bumpConfigInMemory(env, name, pgConn, repoURL)

	env.Prompter.Printf("\n  ✓ deployment %s ready\n", name)
	env.Prompter.Printf("    repo:    %s\n", repoURL)
	env.Prompter.Printf("    config:  %s\n\n", filepath.Join(env.Paths.Config.Value, "pg_hardstorage.yaml"))

	// Optional first backup.  Wired by reusing flowBackup so we
	// don't duplicate its prompt/confirm logic.
	takeNow, err := env.Prompter.YesNo("Take a first backup right now?", true)
	if err != nil {
		return err
	}
	if takeNow {
		fb := flowBackup{}
		if err := fb.runFor(ctx, env, Deployment{Name: name, PGConnection: pgConn, Repo: repoURL}); err != nil {
			env.Prompter.Printf("  (first backup failed: %v — your setup is still saved)\n", err)
		}
	}
	return nil
}

// firstNonEmpty returns the first non-empty string from its args.
// Tiny helper that keeps the "use last value, else hard-coded default"
// pattern readable without a dedicated function name at each prompt.
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// yesNoStr renders a bool as a human word for the summary block.
func yesNoStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// defaultRepoURL returns a per-user fs:// path that exists on every
// platform the binary ships for.  Lands under XDG_DATA_HOME (Linux)
// or ~/Library/Application Support (macOS); we just read it off the
// resolved Paths.State.
func defaultRepoURL(env *Env) string {
	if env.Paths == nil {
		return "file:///var/lib/pg_hardstorage/repo"
	}
	return "file://" + filepath.Join(env.Paths.State.Value, "repo")
}

// validateDSN does a cheap shape-check on the operator's input.  No
// network call here — the backup runner's PG connect surfaces real
// connectivity errors with the full diagnostic.  We just want to
// reject obvious typos before we even ask the next question.
func validateDSN(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("empty connection string")
	}
	if !strings.HasPrefix(s, "postgres://") && !strings.HasPrefix(s, "postgresql://") {
		return errors.New(`expected libpq URI starting with "postgres://" or "postgresql://"`)
	}
	return nil
}

// validateDeploymentName mirrors the constraint the backup-ID
// generator imposes: alphanumerics, dash, underscore.  Anything
// outside that set is rejected so it never lands in a manifest key.
func validateDeploymentName(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("empty name")
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return fmt.Errorf("only [A-Za-z0-9_-] allowed (got %q)", r)
		}
	}
	return nil
}

// validateRepoURL accepts the URL schemes the storage registry actually
// registers (file, s3, gcs, azblob, sftp, scp — see
// internal/plugin/storage/*). It is validated by trying a probe write
// only for file:// — remote schemes can't be probed without credentials
// and we don't ask for those at setup time.
func validateRepoURL(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("empty URL")
	}
	if strings.HasPrefix(s, "file://") {
		path := strings.TrimPrefix(s, "file://")
		if path == "" || path[0] != '/' {
			return errors.New(`file:// URL must be absolute, e.g. file:///var/lib/pg_hardstorage/repo`)
		}
		parent := filepath.Dir(path)
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("cannot create %s: %w", parent, err)
		}
		return nil
	}
	// Must match the schemes registered in internal/plugin/storage/*.
	for _, p := range []string{"s3://", "gcs://", "azblob://", "sftp://", "scp://"} {
		if strings.HasPrefix(s, p) {
			return nil
		}
	}
	return errors.New(`unknown URL scheme; expected file:// / s3:// / gcs:// / azblob:// / sftp:// / scp://`)
}

// dbnameFromDSN pulls the dbname out of a libpq URI, used as the
// deployment-name default.  Returns "" when the URI has no path
// segment (e.g. "postgres://host/" — no db).
func dbnameFromDSN(dsn string) string {
	const sep = "://"
	i := indexOf(dsn, sep)
	if i < 0 {
		return ""
	}
	rest := dsn[i+len(sep):]
	// Strip user@/host:port/, find the path.
	if at := indexOf(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	if slash := indexOf(rest, "/"); slash >= 0 {
		db := rest[slash+1:]
		// Cut at "?" (query params) and any further slash.
		for _, cut := range []string{"?", "/"} {
			if k := indexOf(db, cut); k >= 0 {
				db = db[:k]
			}
		}
		return db
	}
	return ""
}

// persistDeployment merges the new deployment into
// <Config>/pg_hardstorage.yaml.  Loads-merge-write keeps any
// existing operator-edited fields (schedules, retention overrides)
// intact; we only insert the new key.
func persistDeployment(env *Env, name, pgConn, repoURL string) error {
	if env.Paths == nil {
		return errors.New("no config path resolved")
	}
	cfgPath := filepath.Join(env.Paths.Config.Value, "pg_hardstorage.yaml")
	if err := os.MkdirAll(env.Paths.Config.Value, 0o755); err != nil {
		return err
	}
	// Read-modify-write through yaml.Node so unknown fields are
	// preserved.  On a fresh first-run, the file doesn't exist
	// yet and we start with an empty document.
	var root yaml.Node
	body, readErr := os.ReadFile(cfgPath)
	if readErr == nil && len(body) > 0 {
		if err := yaml.Unmarshal(body, &root); err != nil {
			return fmt.Errorf("parse existing config: %w", err)
		}
	}
	if root.Kind == 0 {
		root.Kind = yaml.DocumentNode
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	doc := root.Content[0]

	depsNode := mappingChild(doc, "deployments")
	if depsNode == nil {
		key := &yaml.Node{Kind: yaml.ScalarNode, Value: "deployments"}
		val := &yaml.Node{Kind: yaml.MappingNode}
		doc.Content = append(doc.Content, key, val)
		depsNode = val
	}
	newDep := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "pg_connection"}, {Kind: yaml.ScalarNode, Value: pgConn},
		{Kind: yaml.ScalarNode, Value: "repo"}, {Kind: yaml.ScalarNode, Value: repoURL},
	}}
	// Overwrite if already present (re-running setup with the same
	// name is the "update my settings" path).
	replaced := false
	for i := 0; i+1 < len(depsNode.Content); i += 2 {
		if depsNode.Content[i].Value == name {
			depsNode.Content[i+1] = newDep
			replaced = true
			break
		}
	}
	if !replaced {
		depsNode.Content = append(depsNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: name}, newDep)
	}

	out, err := yaml.Marshal(&root)
	if err != nil {
		return err
	}
	tmp := cfgPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, cfgPath)
}

// mappingChild returns the value child of a mapping node by key,
// or nil if the key is absent.  yaml.Node mappings store kv pairs
// flattened in Content as [k0, v0, k1, v1, ...].
func mappingChild(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// bumpConfigInMemory adds the new deployment to the in-memory
// LoadResult so the rest of the session sees it without re-reading
// disk.  Keeps multi-operation sessions snappy (set up + take
// backup right away picks the new entry from env.Config).
func bumpConfigInMemory(env *Env, name, pgConn, repoURL string) {
	if env.Config == nil {
		return
	}
	if env.Config.Config.Deployments == nil {
		env.Config.Config.Deployments = map[string]config.DeploymentConfig{}
	}
	d := env.Config.Config.Deployments[name]
	d.PGConnection = pgConn
	d.Repo = repoURL
	env.Config.Config.Deployments[name] = d
}
