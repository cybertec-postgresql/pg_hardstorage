// Package cost computes a per-deployment / per-repo cost report
// against a pg_hardstorage repository. v0.1 produces:
//
//   - Total physical bytes in the repo (chunks + manifests + WAL + audit)
//   - Per-deployment manifest bytes
//   - Per-deployment WAL bytes (wal/<deployment>/...)
//   - Per-deployment logical bytes (sum of FileEntry.Size from each
//     deployment's committed manifests — pre-dedup, pre-compression)
//   - Estimated monthly cost = total_physical * price_per_gb_month
//
// What's deliberately approximate: per-deployment *chunk* bytes.
// Chunks are content-addressed and shared across backups via dedup,
// so a precise per-deployment chunk attribution would require walking
// the full reference graph and apportioning multi-referenced chunks.
// The honest v0.1 cut reports total chunk bytes once, separately, and
// shows each deployment's logical+manifest+WAL footprint — sufficient
// for the operator's "where is my repo budget going?" question.
package cost

import (
	"context"
	"encoding/json"
	stdio "io"
	"sort"
	"strings"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// DefaultPricePerGBMonth is the per-GB-month USD rate applied when the
// caller doesn't pass a custom value. AWS S3 Standard, us-east-1,
// posted Q2 2026. Updates land alongside CHANGELOG entries.
const DefaultPricePerGBMonth = 0.023

// Report is the structured output of Compute.
type Report struct {
	Schema              string           `json:"schema"`
	RepoURL             string           `json:"repo_url"`
	PricePerGBMonth     float64          `json:"price_per_gb_month_usd"`
	TotalPhysicalBytes  int64            `json:"total_physical_bytes"`
	ChunkBytes          int64            `json:"chunk_bytes"`
	WALBytes            int64            `json:"wal_bytes"`
	ManifestBytes       int64            `json:"manifest_bytes"`
	AuditBytes          int64            `json:"audit_bytes"`
	EstimatedMonthlyUSD float64          `json:"estimated_monthly_usd"`
	Deployments         []DeploymentCost `json:"deployments"`
}

// SchemaCost is the JSON schema string. Same 24-month back-compat
// commitment as everything else.
const SchemaCost = "pg_hardstorage.cost.v1"

// DeploymentCost is one deployment's slice. Logical = pre-dedup
// FileEntry sizes summed from manifests; Manifest + WAL are exact
// physical bytes for the deployment-prefixed keys.
type DeploymentCost struct {
	Name          string `json:"name"`
	BackupCount   int    `json:"backup_count"`
	LogicalBytes  int64  `json:"logical_bytes"`
	ManifestBytes int64  `json:"manifest_bytes"`
	WALBytes      int64  `json:"wal_bytes"`
}

// Compute walks sp, tallies the categories, and returns the Report.
func Compute(ctx context.Context, sp storage.StoragePlugin, repoURL string, priceUSD float64) (*Report, error) {
	if priceUSD <= 0 {
		priceUSD = DefaultPricePerGBMonth
	}

	r := &Report{
		Schema:          SchemaCost,
		RepoURL:         repoURL,
		PricePerGBMonth: priceUSD,
	}

	// 1. Top-level prefix tally — chunks/manifests/wal/audit. The
	// shape mirrors `repo usage` so the two reports always agree on
	// the physical totals (single source of truth would be a shared
	// helper; for v0.1 we duplicate the tiny walk and let drift land
	// in CI as a test diff if either ever changes).
	roots := []struct {
		prefix string
		dst    *int64
	}{
		{"chunks/", &r.ChunkBytes},
		{"manifests/", &r.ManifestBytes},
		{"wal/", &r.WALBytes},
		{"audit/", &r.AuditBytes},
	}
	for _, root := range roots {
		for info, err := range sp.List(ctx, root.prefix) {
			if err != nil {
				return nil, err
			}
			*root.dst += info.Size
		}
	}
	r.TotalPhysicalBytes = r.ChunkBytes + r.ManifestBytes + r.WALBytes + r.AuditBytes
	r.EstimatedMonthlyUSD = float64(r.TotalPhysicalBytes) / (1024 * 1024 * 1024) * priceUSD

	// 2. Per-deployment slice. For each deployment, walk its prefixes
	// once and tally manifest + WAL bytes; for logical bytes, parse
	// each manifest body to sum FileEntry.Size.
	ms := backup.NewManifestStore(sp)
	deployments, err := ms.Deployments(ctx)
	if err != nil {
		return nil, err
	}
	for _, dep := range deployments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dc := DeploymentCost{Name: dep}

		// Manifest bytes: walk manifests/<dep>/, classify, sum.
		manifestPrefix := "manifests/" + dep + "/"
		for info, err := range sp.List(ctx, manifestPrefix) {
			if err != nil {
				return nil, err
			}
			dc.ManifestBytes += info.Size
			if strings.HasSuffix(info.Key, "/manifest.json") &&
				!strings.Contains(info.Key, "manifest.json.tmp") {
				dc.BackupCount++
			}
		}

		// WAL bytes: walk wal/<dep>/, sum everything (segment
		// manifests and any future per-deployment metadata).
		for info, err := range sp.List(ctx, "wal/"+dep+"/") {
			if err != nil {
				return nil, err
			}
			dc.WALBytes += info.Size
		}

		// Logical bytes: read every committed manifest and sum file
		// sizes.  ListAttestationless skips signature verification —
		// this is a cost report, not an integrity check; trusting
		// the JSON is fine.  Previously this passed nil to ms.List
		// which rejected every signed manifest as "nil verifier",
		// so cost report misreported $0 against any repo with
		// signed backups.
		for m, err := range ms.ListAttestationless(ctx, dep) {
			if err != nil {
				continue
			}
			if m == nil {
				continue
			}
			for _, f := range m.Files {
				dc.LogicalBytes += f.Size
			}
		}

		r.Deployments = append(r.Deployments, dc)
	}

	// Stable order — name ASC.
	sort.Slice(r.Deployments, func(i, j int) bool {
		return r.Deployments[i].Name < r.Deployments[j].Name
	})

	return r, nil
}

// Marshal returns r as JSON bytes (with stable indentation suitable
// for diffing in tests).
func (r *Report) Marshal() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// WriteJSON streams the report as a single JSON document.
func (r *Report) WriteJSON(w stdio.Writer) error {
	body, err := r.Marshal()
	if err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}
