// llm_skill_rollback.go — CLI surface for installing and rolling back LLM skills with snapshot history.
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/skills"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// newLlmSkillInstallCmd implements `pg_hardstorage llm skill install
// <path>`.  Copies a skill YAML file into the operator-overlay
// directory (`/etc/pg_hardstorage/skills/`, or
// $PG_HARDSTORAGE_SKILL_DIR for tests), snapshotting any existing
// file under the same skill name as `<name>.skill.yaml.<rfc3339>`
// so `llm skill rollback` has something to revert to.
//
// We deliberately don't trust the source path's filename — we
// LOAD the YAML, read its `name:` field, and write to
// `<targetdir>/<name>.skill.yaml`.  This prevents the install
// path from being subverted by a sneaky filename like
// `../etc/passwd.skill.yaml` and keeps rollback's lookup
// table-stakes simple.
func newLlmSkillInstallCmd() *cobra.Command {
	var targetDir string
	c := &cobra.Command{
		Use:          "install <path>",
		Short:        "Install a skill YAML file into the operator overlay (snapshots any existing version for rollback)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := DispatcherFrom(cmd)
			source := args[0]

			// Load + validate before we touch the filesystem.
			s, err := skills.LoadFile(source)
			if err != nil {
				return output.NewError("llm.skill_load_failed",
					fmt.Sprintf("llm skill install: %v", err)).Wrap(err)
			}
			if issues := s.Lint(); len(issues) > 0 {
				return output.NewError("llm.skill_lint_failed",
					fmt.Sprintf("llm skill install: refusing to install a lint-failing skill (%d issues)", len(issues))).
					WithSuggestion(&output.Suggestion{
						Human:   "fix the lint issues, then re-run install",
						Command: fmt.Sprintf("pg_hardstorage llm skill lint %s", source),
					}).Wrap(output.ErrUsage)
			}
			if targetDir == "" {
				targetDir = resolveSkillTargetDir()
			}
			if err := os.MkdirAll(targetDir, 0o755); err != nil {
				return output.NewError("llm.skill_install_failed",
					fmt.Sprintf("llm skill install: mkdir %s: %v", targetDir, err)).Wrap(err)
			}

			finalPath := filepath.Join(targetDir, s.Name+".skill.yaml")

			// Snapshot the existing file (if any) under a
			// time-stamped name so rollback has a previous version
			// to revert to.  The snapshot timestamp is RFC3339
			// without colons (filename-safe across every FS we
			// support).
			snapshotPath := ""
			if _, statErr := os.Stat(finalPath); statErr == nil {
				snapshotPath = filepath.Join(targetDir, snapshotFilename(s.Name, time.Now().UTC()))
				if err := copyFile(finalPath, snapshotPath); err != nil {
					return output.NewError("llm.skill_install_failed",
						fmt.Sprintf("llm skill install: snapshot %s: %v", finalPath, err)).Wrap(err)
				}
			}

			if err := copyFile(source, finalPath); err != nil {
				return output.NewError("llm.skill_install_failed",
					fmt.Sprintf("llm skill install: write %s: %v", finalPath, err)).Wrap(err)
			}

			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(skillInstallBody{
				Name:         s.Name,
				Version:      s.Version,
				InstalledAt:  finalPath,
				SnapshotPath: snapshotPath,
			}))
		},
	}
	c.Flags().StringVar(&targetDir, "dir", "",
		"override the install directory (default: /etc/pg_hardstorage/skills/, or $PG_HARDSTORAGE_SKILL_DIR if set)")
	return c
}

// newLlmSkillRollbackCmd implements `pg_hardstorage llm skill
// rollback <name>`.  Finds the most recent snapshot of
// `<name>.skill.yaml` under the operator-overlay directory and
// swaps it back into place, archiving the current file under a
// fresh snapshot so the rollback itself is reversible.
func newLlmSkillRollbackCmd() *cobra.Command {
	var targetDir string
	c := &cobra.Command{
		Use:          "rollback <name>",
		Short:        "Revert a skill to its previous installed version",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := DispatcherFrom(cmd)
			name := args[0]
			if targetDir == "" {
				targetDir = resolveSkillTargetDir()
			}

			snapshots, err := listSkillSnapshots(targetDir, name)
			if err != nil {
				return output.NewError("llm.skill_rollback_failed",
					fmt.Sprintf("llm skill rollback: %v", err)).Wrap(err)
			}
			if len(snapshots) == 0 {
				return output.NewError("notfound.skill_snapshot",
					fmt.Sprintf("llm skill rollback: no previous version of %q under %s — nothing to roll back to", name, targetDir)).
					WithSuggestion(&output.Suggestion{
						Human: "snapshots are created by `llm skill install`; if no install has run yet, there's nothing to revert",
					})
			}

			// Newest snapshot = the most recent previous version.
			latest := snapshots[len(snapshots)-1]
			finalPath := filepath.Join(targetDir, name+".skill.yaml")

			// Archive current (if any) under a fresh snapshot so
			// `rollback` is itself reversible by another rollback.
			postSnapshot := ""
			if _, statErr := os.Stat(finalPath); statErr == nil {
				postSnapshot = filepath.Join(targetDir, snapshotFilename(name, time.Now().UTC()))
				if err := copyFile(finalPath, postSnapshot); err != nil {
					return output.NewError("llm.skill_rollback_failed",
						fmt.Sprintf("llm skill rollback: archive current %s: %v", finalPath, err)).Wrap(err)
				}
			}
			if err := copyFile(latest, finalPath); err != nil {
				return output.NewError("llm.skill_rollback_failed",
					fmt.Sprintf("llm skill rollback: restore %s from %s: %v", finalPath, latest, err)).Wrap(err)
			}
			// Remove the snapshot we just rolled FROM — otherwise
			// repeated rollbacks would oscillate between two versions.
			if err := os.Remove(latest); err != nil {
				// Non-fatal: the new file is in place, the operator
				// just has one extra snapshot file lying around.
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not remove snapshot %s: %v\n", latest, err)
			}

			// Re-load to surface the version the operator just
			// rolled BACK to in the result.
			s, err := skills.LoadFile(finalPath)
			if err != nil {
				return output.NewError("llm.skill_rollback_failed",
					fmt.Sprintf("llm skill rollback: re-read %s: %v", finalPath, err)).Wrap(err)
			}

			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(skillRollbackBody{
				Name:                   s.Name,
				NowInstalledVersion:    s.Version,
				RestoredFromSnapshot:   latest,
				PostRollbackSnapshot:   postSnapshot,
				FinalPath:              finalPath,
				RemainingSnapshotCount: len(snapshots) - 1,
			}))
		},
	}
	c.Flags().StringVar(&targetDir, "dir", "",
		"override the install directory (default: /etc/pg_hardstorage/skills/, or $PG_HARDSTORAGE_SKILL_DIR if set)")
	return c
}

// newLlmSkillHistoryCmd implements `pg_hardstorage llm skill
// history <name>` — lists every snapshot we have for a skill.
func newLlmSkillHistoryCmd() *cobra.Command {
	var targetDir string
	c := &cobra.Command{
		Use:          "history <name>",
		Short:        "List installed snapshots for a skill (most recent last)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := DispatcherFrom(cmd)
			name := args[0]
			if targetDir == "" {
				targetDir = resolveSkillTargetDir()
			}
			snaps, err := listSkillSnapshots(targetDir, name)
			if err != nil {
				return output.NewError("llm.skill_history_failed",
					fmt.Sprintf("llm skill history: %v", err)).Wrap(err)
			}
			if snaps == nil {
				snaps = []string{}
			}
			body := skillHistoryBody{
				Name:      name,
				Dir:       targetDir,
				Snapshots: snaps,
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
		},
	}
	c.Flags().StringVar(&targetDir, "dir", "",
		"override the install directory")
	return c
}

// resolveSkillTargetDir picks the directory `install` writes to
// and `rollback` reads from.  $PG_HARDSTORAGE_SKILL_DIR overrides
// the FHS default — the same env var the loader honours, so a
// rollback in test mode reaches the same directory the loader
// found the skill in.
func resolveSkillTargetDir() string {
	if d := os.Getenv("PG_HARDSTORAGE_SKILL_DIR"); d != "" {
		return d
	}
	return "/etc/pg_hardstorage/skills"
}

// snapshotFilename returns the filename a snapshot of <name> at
// <when> lives under.  Format:
// `<name>.skill.yaml.<rfc3339-with-colons-replaced>.<nanos>`.
//
// We include sub-second nanos because automated workflows do
// install N versions in quick succession (think: `make install`
// regenerating a skill every keystroke); a 1-second resolution
// would lose snapshots to filename collisions.  Sortable
// lexicographically up to 9-digit nanos.
func snapshotFilename(name string, when time.Time) string {
	stamp := strings.ReplaceAll(when.Format(time.RFC3339), ":", "-")
	return fmt.Sprintf("%s.skill.yaml.%s.%09d", name, stamp, when.Nanosecond())
}

// listSkillSnapshots returns the snapshot files for <name>,
// sorted oldest-first.  An empty list is the "no snapshots"
// response, not an error.
func listSkillSnapshots(dir, name string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	prefix := name + ".skill.yaml."
	var snaps []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		// Defensive: if `n` matches the prefix exactly we'd be
		// looking at the active file, not a snapshot.  ReadDir
		// only returns the entry's name; the active skill file
		// is `<name>.skill.yaml` (no trailing suffix), which
		// fails the prefix-with-dot check.
		snaps = append(snaps, filepath.Join(dir, n))
	}
	sort.Strings(snaps)
	return snaps, nil
}

// copyFile copies src to dst. It reads the whole source BEFORE
// touching dst and writes via a temp-file+rename, so:
//
//   - re-installing a skill from its own installed path (src == dst)
//     no longer truncates the file with O_TRUNC before it is read; and
//   - a crash mid-copy leaves either the old or the new file, never a
//     torn half-written one.
//
// fsutil.WriteFileAtomic handles the tmp+fsync+rename+dir-fsync dance.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, statErr := os.Stat(src); statErr == nil {
		mode = fi.Mode().Perm()
	}
	// WriteFileAtomic opens the tmp with O_EXCL; clear any stale tmp
	// from a prior crashed write so a re-install doesn't wedge.
	_ = os.Remove(dst + ".tmp")
	return fsutil.WriteFileAtomic(dst, data, mode)
}

// --- result bodies (v1 schema) ----------------------------------------

type skillInstallBody struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	InstalledAt  string `json:"installed_at"`
	SnapshotPath string `json:"snapshot_path,omitempty"`
}

// WriteText renders the install result — including any snapshot of the
// previous version — as human-readable text to w.
func (b skillInstallBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ installed skill %q v%s at %s\n", b.Name, b.Version, b.InstalledAt)
	if b.SnapshotPath != "" {
		fmt.Fprintf(bw, "  snapshotted previous version at %s (revert with `pg_hardstorage llm skill rollback %s`)", b.SnapshotPath, b.Name)
	} else {
		fmt.Fprintf(bw, "  no previous version — first install of this skill")
	}
	_, err := io.WriteString(w, bw.String())
	return err
}

type skillRollbackBody struct {
	Name                   string `json:"name"`
	NowInstalledVersion    string `json:"now_installed_version"`
	RestoredFromSnapshot   string `json:"restored_from_snapshot"`
	PostRollbackSnapshot   string `json:"post_rollback_snapshot,omitempty"`
	FinalPath              string `json:"final_path"`
	RemainingSnapshotCount int    `json:"remaining_snapshot_count"`
}

// WriteText renders the rollback outcome — restored version and remaining
// snapshot count — as human-readable text to w.
func (b skillRollbackBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "✓ rolled %q back to v%s\n", b.Name, b.NowInstalledVersion)
	fmt.Fprintf(bw, "  Restored from: %s\n", b.RestoredFromSnapshot)
	if b.PostRollbackSnapshot != "" {
		fmt.Fprintf(bw, "  Pre-rollback file archived as: %s\n", b.PostRollbackSnapshot)
	}
	fmt.Fprintf(bw, "  Active file:   %s\n", b.FinalPath)
	fmt.Fprintf(bw, "  Remaining snapshots: %d", b.RemainingSnapshotCount)
	_, err := io.WriteString(w, bw.String())
	return err
}

type skillHistoryBody struct {
	Name      string   `json:"name"`
	Dir       string   `json:"dir"`
	Snapshots []string `json:"snapshots"`
}

// WriteText renders the available snapshot history for a skill as
// human-readable text to w.
func (b skillHistoryBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if len(b.Snapshots) == 0 {
		fmt.Fprintf(bw, "No snapshots for %q under %s", b.Name, b.Dir)
	} else {
		fmt.Fprintf(bw, "Snapshots for %q (oldest first):\n", b.Name)
		for _, s := range b.Snapshots {
			fmt.Fprintf(bw, "  %s\n", s)
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}
