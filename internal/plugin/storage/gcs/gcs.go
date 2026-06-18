// Package gcs implements storage.StoragePlugin over Google
// Cloud Storage (GCS).
//
// URL form:
//
//	gcs://<bucket>[/<prefix>][?option=value&...]
//
// Examples:
//
//	gcs://acme-pg-backups
//	gcs://acme-pg-backups/prod/db1
//	gcs://acme-pg-backups?storage_class=NEARLINE
//
// # Authentication
//
// `storage.NewClient` reads Application Default Credentials
// (ADC) — env vars → metadata service (GCE/GKE/Cloud Run) →
// gcloud auth.  The same chain that gcpkms uses; operators
// running pg_hardstorage on GCP infrastructure get
// metadata-service auth for free.
//
// # Conditional writes
//
// GCS supports `If: storage.Conditions{DoesNotExist: true}`
// on the Object Writer — exactly the IfNotExists semantic
// the StoragePlugin contract needs.  No emulation required;
// concurrent Puts to the same key are resolved atomically
// at the GCS server.
//
// # WORM
//
// GCS Object Lifecycle has a "Bucket Lock" + per-object
// `EventBasedHold` / `TemporaryHold`.  For our SetRetention
// surface we use the per-object `Retention` field
// introduced in 2024 (Object Lock for GCS).  Operators on
// older buckets without retention configured get
// `ErrUnsupported` from SetRetention; the rest of the
// plugin still works.
//
// # Air-gap
//
// GCS is reachable via Private Google Access from VPCs.
// The default endpoint is the public Google host;
// operators using PGA / private-cluster Workload Identity
// configure a `endpoint=` URL parameter that the air-gap
// policy will accept (private IP only in strict mode).
package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	gcsoption "google.golang.org/api/option"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// init registers the "gcs" URL scheme with the storage
// registry.  Without this, repo.Open never knows how to
// dispatch gcs:// URLs to this plugin.
func init() {
	storage.Register("gcs", func() storage.StoragePlugin { return &Plugin{} })
}

// Plugin is the GCS-backed StoragePlugin.
type Plugin struct {
	storage.NopBarrier // a GCS object PUT is durable on success

	bucket       string
	prefix       string
	storageClass string

	mu     sync.Mutex
	client *gcs.Client
	closed bool
}

// Name implements storage.StoragePlugin.
func (p *Plugin) Name() string { return "gcs" }

// Capabilities implements storage.StoragePlugin.
func (p *Plugin) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		ConditionalPut: true, // native via If: Conditions{DoesNotExist:true}
		InlineDurable:  true, // a successful object PUT is durable
	}
}

// Open implements storage.StoragePlugin.
func (p *Plugin) Open(ctx context.Context, cfg storage.StorageConfig) error {
	if cfg.URL == nil {
		return errors.New("gcs: nil URL")
	}
	bucket, prefix, err := parseGCSURL(cfg.URL)
	if err != nil {
		return err
	}
	q := cfg.URL.Query()
	endpoint := q.Get("endpoint")
	credsFile := q.Get("credentials_file")
	if credsFile == "" {
		if v, ok := cfg.Extras["credentials_file"]; ok {
			credsFile = v
		}
	}
	storageClass := q.Get("storage_class")

	if endpoint != "" {
		if err := airgap.Default().EndpointAllowed(endpoint); err != nil {
			return fmt.Errorf("gcs: %w", err)
		}
	}

	var opts []gcsoption.ClientOption
	if endpoint != "" {
		opts = append(opts, gcsoption.WithEndpoint(endpoint))
	}
	if credsFile != "" {
		opts = append(opts, gcsoption.WithCredentialsFile(credsFile))
	}
	cli, err := gcs.NewClient(ctx, opts...)
	if err != nil {
		return fmt.Errorf("gcs: open client: %w", err)
	}
	p.bucket = bucket
	p.prefix = prefix
	p.storageClass = storageClass
	p.client = cli
	return nil
}

// Close implements storage.StoragePlugin.  Idempotent.
func (p *Plugin) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if p.client != nil {
		err := p.client.Close()
		p.client = nil
		return err
	}
	return nil
}

func (p *Plugin) fullKey(key string) string {
	if p.prefix == "" {
		return key
	}
	return p.prefix + "/" + key
}

// stripPrefix is the inverse of fullKey: maps a server-side
// object name back to the repo-relative key the caller
// supplied.
func (p *Plugin) stripPrefix(name string) string {
	if p.prefix == "" {
		return name
	}
	return strings.TrimPrefix(name, p.prefix+"/")
}

// Put implements storage.StoragePlugin.
func (p *Plugin) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if err := p.assertOpen(); err != nil {
		return storage.PutResult{}, err
	}
	obj := p.client.Bucket(p.bucket).Object(p.fullKey(key))
	if opts.IfNotExists {
		obj = obj.If(gcs.Conditions{DoesNotExist: true})
	}
	w := obj.NewWriter(ctx)
	if p.storageClass != "" {
		w.StorageClass = p.storageClass
	}
	written, err := io.Copy(w, r)
	if closeErr := w.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		if isPreconditionFailed(err) {
			return storage.PutResult{}, storage.ErrAlreadyExists
		}
		return storage.PutResult{}, fmt.Errorf("gcs: put %s: %w", key, err)
	}
	// Apply WORM retention at PUT time. The CAS's chunk path carries the
	// deadline in PutOptions.RetainUntil and relies on Put to enforce it —
	// only the manifest path makes a separate SetRetention call. Without
	// this, every WORM-configured CHUNK (the bulk of a backup's bytes) was
	// written to GCS with no Object Retention, leaving the "compliance"
	// backup freely deletable. (s3 already applied retention inline; gcs
	// silently didn't.)
	if !opts.RetainUntil.IsZero() {
		mode := opts.RetentionMode
		if mode == "" {
			mode = storage.WORMCompliance // regulatory-grade default, matching s3
		}
		if rerr := p.SetRetention(ctx, key, opts.RetainUntil, mode); rerr != nil {
			// Uploaded but not locked. Delete it so a retry re-uploads and
			// re-locks rather than leaving an UNLOCKED object a dedup hit
			// (IfNotExists) would treat as a committed, protected object.
			_ = p.Delete(ctx, key)
			return storage.PutResult{}, fmt.Errorf("gcs: apply retention to %s: %w", key, rerr)
		}
	}
	return storage.PutResult{Key: key, Size: written}, nil
}

// Get implements storage.StoragePlugin.
func (p *Plugin) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := p.assertOpen(); err != nil {
		return nil, err
	}
	r, err := p.client.Bucket(p.bucket).Object(p.fullKey(key)).NewReader(ctx)
	if err != nil {
		if errors.Is(err, gcs.ErrObjectNotExist) {
			return nil, storage.ErrNotFound
		}
		return nil, fmt.Errorf("gcs: get %s: %w", key, err)
	}
	return r, nil
}

// Stat implements storage.StoragePlugin.
func (p *Plugin) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if err := p.assertOpen(); err != nil {
		return storage.ObjectInfo{}, err
	}
	attrs, err := p.client.Bucket(p.bucket).Object(p.fullKey(key)).Attrs(ctx)
	if err != nil {
		if errors.Is(err, gcs.ErrObjectNotExist) {
			return storage.ObjectInfo{}, storage.ErrNotFound
		}
		return storage.ObjectInfo{}, fmt.Errorf("gcs: stat %s: %w", key, err)
	}
	info := storage.ObjectInfo{
		Key:          key,
		Size:         attrs.Size,
		ModTime:      attrs.Updated,
		ETag:         attrs.Etag,
		StorageClass: attrs.StorageClass,
	}
	return info, nil
}

// List implements storage.StoragePlugin.
func (p *Plugin) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	return func(yield func(storage.ObjectInfo, error) bool) {
		if err := p.assertOpen(); err != nil {
			yield(storage.ObjectInfo{}, err)
			return
		}
		fullPrefix := p.fullKey(prefix)
		it := p.client.Bucket(p.bucket).Objects(ctx, &gcs.Query{Prefix: fullPrefix})
		for {
			if err := ctx.Err(); err != nil {
				yield(storage.ObjectInfo{}, err)
				return
			}
			attrs, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return
			}
			if err != nil {
				yield(storage.ObjectInfo{}, fmt.Errorf("gcs: list %s: %w", prefix, err))
				return
			}
			info := storage.ObjectInfo{
				Key:          p.stripPrefix(attrs.Name),
				Size:         attrs.Size,
				ModTime:      attrs.Updated,
				ETag:         attrs.Etag,
				StorageClass: attrs.StorageClass,
			}
			if !yield(info, nil) {
				return
			}
		}
	}
}

// Delete implements storage.StoragePlugin.  Removing a non-
// existent key is a no-op (matches the contract — retried
// deletes are safe).
func (p *Plugin) Delete(ctx context.Context, key string) error {
	if err := p.assertOpen(); err != nil {
		return err
	}
	err := p.client.Bucket(p.bucket).Object(p.fullKey(key)).Delete(ctx)
	if err == nil {
		return nil
	}
	if errors.Is(err, gcs.ErrObjectNotExist) {
		return nil
	}
	return fmt.Errorf("gcs: delete %s: %w", key, err)
}

// RenameIfNotExists implements storage.StoragePlugin via
// copy + delete.  GCS doesn't have a native atomic rename;
// the copy carries `DoesNotExist: true` so concurrent
// renamers see exactly one winner.
func (p *Plugin) RenameIfNotExists(ctx context.Context, src, dst string) error {
	if err := p.assertOpen(); err != nil {
		return err
	}
	bucket := p.client.Bucket(p.bucket)
	srcObj := bucket.Object(p.fullKey(src))
	dstFullKey := p.fullKey(dst)

	// Defence in depth: Attrs(dst) FIRST and refuse if it
	// exists.  Without this we relied on
	// `gcs.Conditions{DoesNotExist: true}` on the Copy;
	// real GCS honours the precondition but fake-gcs-server
	// (and likely some other GCS-compat backends) silently
	// IGNORES it and proceeds with the overwrite.  Caught
	// by the contract harness:
	// RenameIfNotExists_DstPresent succeeded and clobbered
	// the existing object — same data-loss class bug as
	// the S3 plugin had against MinIO (fixed earlier in
	// an earlier stack).
	//
	// The Attrs→Copy pattern has an unavoidable race
	// window (a concurrent writer can land between the
	// two calls); the agent's audit-chain serialisation
	// makes it academic in practice.  The DoesNotExist
	// precondition stays on the Copy as a second line of
	// defence on backends that do honour it.
	if _, err := bucket.Object(dstFullKey).Attrs(ctx); err == nil {
		return storage.ErrAlreadyExists
	} else if !errors.Is(err, gcs.ErrObjectNotExist) {
		return fmt.Errorf("gcs: rename precheck attrs %s: %w", dst, err)
	}

	dstObj := bucket.Object(dstFullKey).If(gcs.Conditions{DoesNotExist: true})
	if _, err := dstObj.CopierFrom(srcObj).Run(ctx); err != nil {
		if isPreconditionFailed(err) {
			return storage.ErrAlreadyExists
		}
		if errors.Is(err, gcs.ErrObjectNotExist) {
			return storage.ErrNotFound
		}
		return fmt.Errorf("gcs: rename copy %s → %s: %w", src, dst, err)
	}
	if err := srcObj.Delete(ctx); err != nil && !errors.Is(err, gcs.ErrObjectNotExist) {
		// Copy succeeded but we couldn't delete the source.
		// The dst is in place, so the rename's user-visible
		// effect is correct.  We surface a soft warning via
		// the wrapped error but treat the rename as complete.
		return fmt.Errorf("gcs: rename %s → %s: copied OK but source delete failed: %w", src, dst, err)
	}
	return nil
}

// SetRetention implements storage.StoragePlugin via GCS
// Object Retention.  Maps Compliance / Governance modes
// onto GCS's `Locked` boolean (Compliance = locked,
// Governance = unlocked).
func (p *Plugin) SetRetention(ctx context.Context, key string, until time.Time, mode storage.WORMMode) error {
	if err := p.assertOpen(); err != nil {
		return err
	}
	if mode == storage.WORMNone {
		return nil
	}
	obj := p.client.Bucket(p.bucket).Object(p.fullKey(key))
	retention := &gcs.ObjectRetention{
		Mode:        retentionModeFor(mode),
		RetainUntil: until,
	}
	_, err := obj.Update(ctx, gcs.ObjectAttrsToUpdate{Retention: retention})
	if err != nil {
		return fmt.Errorf("gcs: set retention %s: %w", key, err)
	}
	return nil
}

// retentionModeFor maps our WORMMode onto GCS's mode
// vocabulary.
func retentionModeFor(m storage.WORMMode) string {
	switch m {
	case storage.WORMCompliance:
		return "Locked"
	case storage.WORMGovernance:
		return "Unlocked"
	}
	return ""
}

// isPreconditionFailed detects the "DoesNotExist precondition
// failed" GCS surfaces when an If-NoOverwrite Put hits an
// existing object.  GCS's error type is rich; we match on
// the documented googleapi.Error code.
func isPreconditionFailed(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// GCS errors include the HTTP status; conditional-PUT
	// failure is 412 Precondition Failed.
	return strings.Contains(msg, "412") || strings.Contains(msg, "Precondition") ||
		strings.Contains(msg, "conditionNotMet")
}

func (p *Plugin) assertOpen() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.client == nil {
		return errors.New("gcs: plugin not open")
	}
	return nil
}

// parseGCSURL extracts the bucket + optional prefix from a
// gcs:// URL.
//
//	gcs://<bucket>             → bucket=<bucket>, prefix=""
//	gcs://<bucket>/            → bucket=<bucket>, prefix=""
//	gcs://<bucket>/p/q         → bucket=<bucket>, prefix="p/q"
func parseGCSURL(u *url.URL) (bucket, prefix string, err error) {
	if u.Scheme != "gcs" {
		return "", "", fmt.Errorf("gcs: scheme %q is not gcs", u.Scheme)
	}
	bucket = u.Host
	if bucket == "" {
		return "", "", errors.New("gcs: URL missing bucket (gcs://<bucket>/...)")
	}
	prefix = strings.Trim(path.Clean(u.Path), "/")
	if prefix == "." {
		prefix = ""
	}
	return bucket, prefix, nil
}

// Region implements storage.RegionAware.  GCS objects don't
// expose a per-bucket region in the SDK without an extra
// `bucket.Attrs()` call; we leave Region empty and let the
// repo's residency check rely on the bucket's location
// being out-of-band-known.
func (p *Plugin) Region() string { return "" }

// silence unused-import lints for time + io in case a
// future refactor strips the only direct use.
var (
	_           = time.Time{}
	_ io.Reader = (*strings.Reader)(nil)
)
