// Package reproducer dumps a forensic tarball when the soak
// driver hits an assertion failure.  The bundle includes
// everything needed to re-run the exact iteration that broke:
// fleet/profile/fault YAMLs, drive seed, iteration counter,
// per-cell repo metadata + the failing backup's chunks, and a
// `replay.sh` script that re-runs the soak with --seed and
// --resume-iteration.
//
// "Metadata only" means: manifests, attestations, audit chain;
// not the full chunk store (which can be hundreds of GB).
// Operators investigating the failure can fetch additional
// bytes from the original repo if needed.
package reproducer

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Bundle describes everything that goes into the tarball.
// The driver populates it as the soak progresses; on failure
// it's serialised to disk via Write.
type Bundle struct {
	// FailingCell is the cell whose assertion failed.
	FailingCell string

	// Iteration is the loop iteration at which the failure
	// surfaced.
	Iteration int

	// Seed is the drive-loop seed; same seed + same fleet +
	// same iteration → reproducible failure.
	Seed int64

	// FailureMessage is the human-readable error.
	FailureMessage string

	// ProjectName is the docker-compose project name (used
	// by replay.sh to bring up the same containers).
	ProjectName string

	// FleetYAMLPath is the path to the fleet.yaml the run
	// used.  Read at Write() time and embedded in the bundle.
	FleetYAMLPath string

	// ProfilesYAMLPath, FaultsYAMLPath: same.
	ProfilesYAMLPath string
	FaultsYAMLPath   string

	// ComposeYAMLPath: the generated docker-compose.yaml.
	ComposeYAMLPath string

	// MetadataPaths is the list of files / directories whose
	// contents go into the bundle as-is.  Typical entries:
	// the failing cell's manifests/, audit/, anchors.ndjson;
	// the drive's NDJSON event log.
	MetadataPaths []string

	// CreatedAt is stamped onto the bundle's manifest.
	CreatedAt time.Time
}

// Write packs the bundle into out as a gzipped tar.  The
// archive contains:
//
//	bundle-manifest.json     — schema-typed summary of the failure
//	fleet.yaml, profiles.yaml, faults.yaml, docker-compose.yaml
//	metadata/<original-path>  — every entry of MetadataPaths
//	replay.sh                 — runnable resume script
//
// out is closed by the caller; we never close it ourselves so
// the caller can choose between gzip + sha256 + signing layers.
func (b *Bundle) Write(out io.Writer) error {
	if b.FailingCell == "" {
		return errors.New("reproducer: FailingCell is required")
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now().UTC()
	}
	gz := gzip.NewWriter(out)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	// 1. The bundle-manifest is the operator's first read.
	manifest := bundleManifest{
		Schema:         "pg_hardstorage.testkit.reproducer.v1",
		FailingCell:    b.FailingCell,
		Iteration:      b.Iteration,
		Seed:           b.Seed,
		FailureMessage: b.FailureMessage,
		ProjectName:    b.ProjectName,
		CreatedAt:      b.CreatedAt,
	}
	manifestBytes, err := manifest.encode()
	if err != nil {
		return err
	}
	if err := writeTarFile(tw, "bundle-manifest.json", manifestBytes); err != nil {
		return err
	}

	// 2. Inputs that re-create the run.
	for _, in := range []struct{ name, path string }{
		{"fleet.yaml", b.FleetYAMLPath},
		{"profiles.yaml", b.ProfilesYAMLPath},
		{"faults.yaml", b.FaultsYAMLPath},
		{"docker-compose.yaml", b.ComposeYAMLPath},
	} {
		if in.path == "" {
			continue
		}
		body, err := os.ReadFile(in.path)
		if err != nil {
			return fmt.Errorf("reproducer: read %s: %w", in.path, err)
		}
		if err := writeTarFile(tw, in.name, body); err != nil {
			return err
		}
	}

	// 3. Forensic metadata.
	if err := b.writeMetadata(tw); err != nil {
		return err
	}

	// 4. The replay shell script — embeds the seed +
	// iteration so re-runs are reproducible.
	if err := writeTarFile(tw, "replay.sh", []byte(b.replayScript())); err != nil {
		return err
	}
	return nil
}

// WriteToFile is a convenience wrapper that creates path with
// mode 0600 and pipes Write through it.
func (b *Bundle) WriteToFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return b.Write(f)
}

// writeMetadata copies every MetadataPaths entry into the tar
// under metadata/.  Directories are walked recursively.
func (b *Bundle) writeMetadata(tw *tar.Writer) error {
	// Sort so the tar order is deterministic.
	paths := append([]string{}, b.MetadataPaths...)
	sort.Strings(paths)

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			// Missing path is not fatal — log and continue.
			// The bundle still has the YAMLs + replay.sh.
			_ = err
			continue
		}
		base := filepath.Base(p)
		if info.IsDir() {
			err = filepath.WalkDir(p, func(path string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return nil
				}
				if d.IsDir() {
					return nil
				}
				rel, err := filepath.Rel(p, path)
				if err != nil {
					return err
				}
				body, err := os.ReadFile(path)
				if err != nil {
					return nil // best-effort
				}
				return writeTarFile(tw,
					filepath.Join("metadata", base, rel),
					body)
			})
			if err != nil {
				return err
			}
			continue
		}
		body, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if err := writeTarFile(tw,
			filepath.Join("metadata", base), body); err != nil {
			return err
		}
	}
	return nil
}

// replayScript is the shell wrapper users run to reproduce.
// Idempotent: if the docker-compose project is already up, it
// re-uses; otherwise it brings it up from the embedded YAML.
func (b *Bundle) replayScript() string {
	var sb strings.Builder
	sb.WriteString("#!/usr/bin/env bash\n")
	sb.WriteString("# Auto-generated by pg_hardstorage_testkit reproducer.\n")
	sb.WriteString("# Re-runs the exact iteration that surfaced the failure.\n")
	sb.WriteString("set -euo pipefail\n")
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("PROJECT=\"%s\"\n", b.ProjectName))
	sb.WriteString(fmt.Sprintf("CELL=\"%s\"\n", b.FailingCell))
	sb.WriteString(fmt.Sprintf("SEED=%d\n", b.Seed))
	sb.WriteString(fmt.Sprintf("ITER=%d\n", b.Iteration))
	sb.WriteString("\n")
	sb.WriteString("HERE=\"$(cd \"$(dirname \"$0\")\" && pwd)\"\n")
	sb.WriteString("docker compose -p \"$PROJECT\" -f \"$HERE/docker-compose.yaml\" up -d\n")
	sb.WriteString("\n")
	sb.WriteString("pg_hardstorage_testkit validate \\\n")
	sb.WriteString("    --fleet \"$HERE/fleet.yaml\" \\\n")
	sb.WriteString("    --profiles \"$HERE/profiles.yaml\" \\\n")
	sb.WriteString("    --faults \"$HERE/faults.yaml\" \\\n")
	sb.WriteString("    --seed \"$SEED\" \\\n")
	sb.WriteString("    --resume-iteration \"$ITER\" \\\n")
	sb.WriteString("    --only-cell \"$CELL\" \\\n")
	sb.WriteString("    --duration 10m\n")
	return sb.String()
}

// --- helpers ----------------------------------------------------------

func writeTarFile(tw *tar.Writer, name string, body []byte) error {
	hdr := &tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(body)),
		ModTime:  time.Now().UTC(),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("reproducer: tar header %s: %w", name, err)
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("reproducer: tar write %s: %w", name, err)
	}
	return nil
}
