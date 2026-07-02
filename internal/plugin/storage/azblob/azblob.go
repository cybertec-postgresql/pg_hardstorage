// Package azblob implements storage.StoragePlugin over
// Azure Blob Storage.
//
// URL form:
//
//	azblob://<account>/<container>[/<prefix>][?option=value&...]
//
// Examples:
//
//	azblob://acmebackups/prod
//	azblob://acmebackups/prod/db1?access_tier=cool
//	azblob://acmebackups.blob.core.usgovcloudapi.net/prod      # sovereign cloud
//
// The bare-account form gets the public-cloud
// `.blob.core.windows.net` suffix; a dotted account name is
// taken literally so US Government Cloud / Azure China /
// custom domain accounts work without a config flag (same
// pattern as `internal/plugin/kms/azurekv`).
//
// # Authentication
//
// `azblob.NewClient` reads
// `azidentity.NewDefaultAzureCredential`, which chains
// through env vars → managed identity → Azure CLI → IDE
// integrated auth.  Operators on Azure VMs / AKS get
// managed-identity for free.  When the operator wants
// shared-key (storage account key) auth, they pass
// `?account_key=<base64>` in the URL — the plugin builds a
// `SharedKeyCredential` instead.
//
// # Conditional writes
//
// Blob's UploadStream supports the `If-None-Match: *`
// condition (`AccessConditions{ModifiedAccessConditions{
// IfNoneMatch: ETagAny}}`) which atomically refuses an
// overwrite — exactly the IfNotExists semantic the
// StoragePlugin contract needs.  No emulation required.
//
// # WORM
//
// Azure Blob Immutable Storage supports per-blob retention
// policies (with optional Legal Hold).  SetRetention maps
// Compliance / Governance modes onto the
// `Locked` / `Unlocked` immutability policy modes.  Note
// that this requires a vault that has version-level
// immutability enabled at container creation time;
// containers without it return a clear error from the
// service.
package azblob

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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	az "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// init registers the "azblob" URL scheme with the storage
// registry.
func init() {
	storage.Register("azblob", func() storage.StoragePlugin { return &Plugin{} })
}

// Plugin is the Azure Blob Storage-backed StoragePlugin.
type Plugin struct {
	storage.NopBarrier // an Azure Blob PUT is durable on success

	serviceURL string
	container  string
	prefix     string
	accessTier string

	mu     sync.Mutex
	client *az.Client
	closed bool
}

// Name implements storage.StoragePlugin.
func (p *Plugin) Name() string { return "azblob" }

// Capabilities implements storage.StoragePlugin.
func (p *Plugin) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		ConditionalPut: true, // native via If-None-Match: *
		InlineDurable:  true, // a successful blob PUT is durable
	}
}

// Open implements storage.StoragePlugin.
func (p *Plugin) Open(ctx context.Context, cfg storage.StorageConfig) error {
	if cfg.URL == nil {
		return errors.New("azblob: nil URL")
	}
	serviceURL, containerName, prefix, err := parseAzblobURL(cfg.URL)
	if err != nil {
		return err
	}
	q := cfg.URL.Query()
	accessTier := q.Get("access_tier")
	endpoint := q.Get("endpoint")
	accountKey := q.Get("account_key")
	if accountKey == "" {
		if v, ok := cfg.Extras["account_key"]; ok {
			accountKey = v
		}
	}
	// allow_http enables HTTP (non-TLS) endpoints — required
	// for the Azurite emulator, which doesn't ship an HTTPS
	// surface by default.  The Azure SDK refuses to send
	// authenticated requests over HTTP unless this is opt-in.
	// Production deployments should NOT set this — TLS is the
	// only way to keep the SharedKey signature private on the
	// wire.  See internal/testkit/sink/azurite.go for the
	// emulator-driven use.
	allowHTTP := q.Get("allow_http") == "true"

	if endpoint != "" {
		if err := airgap.Default().EndpointAllowed(endpoint); err != nil {
			return fmt.Errorf("azblob: %w", err)
		}
		serviceURL = endpoint
	}
	if err := airgap.Default().EndpointAllowed(serviceURL); err != nil {
		return fmt.Errorf("azblob: %w", err)
	}

	// Build az.ClientOptions only when we need to override the
	// default — the SDK's nil-options path is the canonical
	// "production defaults" mode and we want to stay on it
	// unless the URL explicitly opted into something else.
	var clientOpts *az.ClientOptions
	if allowHTTP {
		clientOpts = &az.ClientOptions{}
		clientOpts.InsecureAllowCredentialWithHTTP = true
	}

	var (
		cli      *az.Client
		buildErr error
	)
	if accountKey != "" {
		// Account name comes from the ORIGINAL azblob://
		// URL's host (the bare account name "acmebackups"
		// or the dotted "acmebackups.blob.core.windows.net"),
		// NOT from the post-override serviceURL.  An
		// endpoint= query that points at a 127.0.0.1
		// emulator (Azurite) destroys the account-from-host
		// derivation but the original URL still carries
		// the right account in u.Host.
		account := cfg.URL.Host
		if i := strings.IndexByte(account, '.'); i > 0 {
			account = account[:i]
		}
		cred, err := az.NewSharedKeyCredential(account, accountKey)
		if err != nil {
			return fmt.Errorf("azblob: build shared-key credential: %w", err)
		}
		cli, buildErr = az.NewClientWithSharedKeyCredential(serviceURL, cred, clientOpts)
	} else {
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return fmt.Errorf("azblob: build credential: %w", err)
		}
		cli, buildErr = az.NewClient(serviceURL, cred, clientOpts)
	}
	if buildErr != nil {
		return fmt.Errorf("azblob: open client: %w", buildErr)
	}

	p.serviceURL = serviceURL
	p.container = containerName
	p.prefix = prefix
	p.accessTier = accessTier
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
	p.client = nil // azblob.Client has no Close()
	return nil
}

// fullKey prefixes the operator-supplied key with the URL's
// optional prefix component.
func (p *Plugin) fullKey(key string) string {
	if p.prefix == "" {
		return key
	}
	return p.prefix + "/" + key
}

// stripPrefix is the inverse — used by List to map the
// server-side blob name back to the repo-relative key.
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
	upOpts := &az.UploadStreamOptions{}
	if opts.IfNotExists {
		upOpts.AccessConditions = &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{
				IfNoneMatch: to.Ptr(azcore.ETagAny),
			},
		}
	}
	if p.accessTier != "" {
		tier := blob.AccessTier(p.accessTier)
		upOpts.AccessTier = &tier
	}
	// Count bytes via a wrapping reader so we can report a
	// non-zero Size; the Azure SDK doesn't return the
	// uploaded byte count directly.
	cw := &countingReader{r: r}
	_, err := p.client.UploadStream(ctx, p.container, p.fullKey(key), cw, upOpts)
	if err != nil {
		if isConditionFailed(err) {
			return storage.PutResult{}, storage.ErrAlreadyExists
		}
		return storage.PutResult{}, fmt.Errorf("azblob: upload %s: %w", key, err)
	}
	// Apply WORM retention at PUT time. The CAS's chunk path carries the
	// deadline in PutOptions.RetainUntil and relies on Put to enforce it —
	// only the manifest path makes a separate SetRetention call. Without
	// this, every WORM-configured CHUNK (the bulk of a backup's bytes) was
	// written to Azure with no immutability policy, leaving the "compliance"
	// backup freely deletable. (s3 already applied retention inline; azblob
	// silently didn't.)
	if !opts.RetainUntil.IsZero() {
		mode := opts.RetentionMode
		if mode == "" {
			mode = storage.WORMCompliance // regulatory-grade default, matching s3
		}
		if rerr := p.SetRetention(ctx, key, opts.RetainUntil, mode); rerr != nil {
			// Uploaded but not locked. Delete it so a retry re-uploads and
			// re-locks rather than leaving an UNLOCKED blob a dedup hit
			// (IfNotExists) would treat as a committed, protected object.
			// Best-effort: if the delete fails because the blob IS locked
			// (a concurrent writer won), it's protected anyway.
			_ = p.Delete(ctx, key)
			return storage.PutResult{}, fmt.Errorf("azblob: apply retention to %s: %w", key, rerr)
		}
	}
	return storage.PutResult{Key: key, Size: cw.n}, nil
}

// Get implements storage.StoragePlugin.
func (p *Plugin) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := p.assertOpen(); err != nil {
		return nil, err
	}
	resp, err := p.client.DownloadStream(ctx, p.container, p.fullKey(key), nil)
	if err != nil {
		if isNotFound(err) {
			return nil, storage.ErrNotFound
		}
		return nil, fmt.Errorf("azblob: download %s: %w", key, err)
	}
	return resp.Body, nil
}

// Stat implements storage.StoragePlugin.  Azure exposes
// blob properties via the per-blob client, which we reach
// through the service client.
func (p *Plugin) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if err := p.assertOpen(); err != nil {
		return storage.ObjectInfo{}, err
	}
	bClient := p.client.ServiceClient().NewContainerClient(p.container).NewBlobClient(p.fullKey(key))
	resp, err := bClient.GetProperties(ctx, nil)
	if err != nil {
		if isNotFound(err) {
			return storage.ObjectInfo{}, storage.ErrNotFound
		}
		return storage.ObjectInfo{}, fmt.Errorf("azblob: stat %s: %w", key, err)
	}
	info := storage.ObjectInfo{Key: key}
	if resp.ContentLength != nil {
		info.Size = *resp.ContentLength
	}
	if resp.LastModified != nil {
		info.ModTime = *resp.LastModified
	}
	if resp.ETag != nil {
		info.ETag = string(*resp.ETag)
	}
	if resp.AccessTier != nil {
		info.StorageClass = *resp.AccessTier
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
		pager := p.client.NewListBlobsFlatPager(p.container, &container.ListBlobsFlatOptions{
			Prefix: to.Ptr(fullPrefix),
		})
		for pager.More() {
			if err := ctx.Err(); err != nil {
				yield(storage.ObjectInfo{}, err)
				return
			}
			page, err := pager.NextPage(ctx)
			if err != nil {
				yield(storage.ObjectInfo{}, fmt.Errorf("azblob: list %s: %w", prefix, err))
				return
			}
			if page.Segment == nil {
				continue
			}
			for _, item := range page.Segment.BlobItems {
				if item == nil || item.Name == nil {
					continue
				}
				info := storage.ObjectInfo{Key: p.stripPrefix(*item.Name)}
				if item.Properties != nil {
					if item.Properties.ContentLength != nil {
						info.Size = *item.Properties.ContentLength
					}
					if item.Properties.LastModified != nil {
						info.ModTime = *item.Properties.LastModified
					}
					if item.Properties.ETag != nil {
						info.ETag = string(*item.Properties.ETag)
					}
					if item.Properties.AccessTier != nil {
						info.StorageClass = string(*item.Properties.AccessTier)
					}
				}
				if !yield(info, nil) {
					return
				}
			}
		}
	}
}

// Delete implements storage.StoragePlugin.  Removing a
// non-existent blob is a no-op (matches the contract).
func (p *Plugin) Delete(ctx context.Context, key string) error {
	if err := p.assertOpen(); err != nil {
		return err
	}
	_, err := p.client.DeleteBlob(ctx, p.container, p.fullKey(key), nil)
	if err == nil {
		return nil
	}
	if isNotFound(err) {
		return nil
	}
	return fmt.Errorf("azblob: delete %s: %w", key, err)
}

// RenameIfNotExists implements storage.StoragePlugin via
// server-side copy + delete.  Azure has no native atomic
// rename; the copy carries the IfNoneMatch condition so
// concurrent renamers see exactly one winner at the
// destination.
func (p *Plugin) RenameIfNotExists(ctx context.Context, src, dst string) error {
	if err := p.assertOpen(); err != nil {
		return err
	}
	srcBlob := p.client.ServiceClient().
		NewContainerClient(p.container).
		NewBlobClient(p.fullKey(src))
	dstBlob := p.client.ServiceClient().
		NewContainerClient(p.container).
		NewBlobClient(p.fullKey(dst))

	srcURL := srcBlob.URL()
	startResp, err := dstBlob.StartCopyFromURL(ctx, srcURL, &blob.StartCopyFromURLOptions{
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{
				IfNoneMatch: to.Ptr(azcore.ETagAny),
			},
		},
	})
	if err != nil {
		if isConditionFailed(err) {
			return storage.ErrAlreadyExists
		}
		if isNotFound(err) {
			return storage.ErrNotFound
		}
		return fmt.Errorf("azblob: rename copy %s → %s: %w", src, dst, err)
	}

	// StartCopyFromURL is ASYNCHRONOUS: the copy may still be pending
	// when the call returns.  Deleting the source before the copy
	// reaches Success would leave the destination absent (a pending
	// copy that later fails/aborts) while rename reported success —
	// silent data loss.  Wait until the copy definitively succeeds
	// before deleting; fail on any non-success terminal state.
	if err := p.awaitCopyComplete(ctx, dstBlob, startResp.CopyStatus); err != nil {
		return fmt.Errorf("azblob: rename copy %s → %s: %w", src, dst, err)
	}

	if _, err := srcBlob.Delete(ctx, nil); err != nil && !isNotFound(err) {
		return fmt.Errorf("azblob: rename %s → %s: copied OK but source delete failed: %w", src, dst, err)
	}
	return nil
}

// copyPollInterval is how long awaitCopyComplete waits between
// GetProperties polls while a server-side copy is pending.  A short
// interval keeps intra-account copies (the common case — they complete
// almost immediately) snappy without hammering the service.
var copyPollInterval = 200 * time.Millisecond

// copyStatusVerdict classifies an Azure copy-status header value.
// Returns (done, err):
//   - done=true,  err=nil        → copy reached Success; safe to delete src
//   - done=true,  err!=nil       → terminal failure (Failed/Aborted); do NOT delete
//   - done=false, err=nil        → still pending/aborting; keep polling
//
// A nil/empty status is treated as pending: some responses omit the
// header until the copy is registered, and treating "unknown" as done
// would risk deleting the source before the copy landed.
func copyStatusVerdict(status *blob.CopyStatusType) (bool, error) {
	if status == nil {
		return false, nil
	}
	switch *status {
	case blob.CopyStatusTypeSuccess:
		return true, nil
	case blob.CopyStatusTypeFailed:
		return true, fmt.Errorf("copy failed (status %q)", *status)
	case blob.CopyStatusTypeAborted:
		return true, fmt.Errorf("copy aborted (status %q)", *status)
	case blob.CopyStatusTypePending:
		return false, nil
	default:
		// Empty header or an unrecognised value: not yet terminal.
		return false, nil
	}
}

// awaitCopyComplete polls the destination blob's copy status until the
// server-side copy reaches a terminal state.  firstStatus is the status
// reported by StartCopyFromURL, checked before the first poll so an
// already-completed synchronous copy skips the GetProperties round-trip.
// Returns nil only when the copy reached Success.
func (p *Plugin) awaitCopyComplete(ctx context.Context, dstBlob *blob.Client, firstStatus *blob.CopyStatusType) error {
	if done, err := copyStatusVerdict(firstStatus); done {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		props, err := dstBlob.GetProperties(ctx, nil)
		if err != nil {
			return fmt.Errorf("poll copy status: %w", err)
		}
		if done, verr := copyStatusVerdict(props.CopyStatus); done {
			return verr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(copyPollInterval):
		}
	}
}

// SetRetention implements storage.StoragePlugin via Azure
// Blob immutability policies.  Compliance → Locked,
// Governance → Unlocked.  WORMNone short-circuits.
func (p *Plugin) SetRetention(ctx context.Context, key string, until time.Time, mode storage.WORMMode) error {
	if err := p.assertOpen(); err != nil {
		return err
	}
	if mode == storage.WORMNone {
		return nil
	}
	bClient := p.client.ServiceClient().
		NewContainerClient(p.container).
		NewBlobClient(p.fullKey(key))
	policy := blob.ImmutabilityPolicySetting(retentionModeFor(mode))
	_, err := bClient.SetImmutabilityPolicy(ctx, until, &blob.SetImmutabilityPolicyOptions{
		Mode: &policy,
	})
	if err != nil {
		return fmt.Errorf("azblob: set retention %s: %w", key, err)
	}
	return nil
}

func retentionModeFor(m storage.WORMMode) blob.ImmutabilityPolicySetting {
	switch m {
	case storage.WORMCompliance:
		return blob.ImmutabilityPolicySettingLocked
	case storage.WORMGovernance:
		return blob.ImmutabilityPolicySettingUnlocked
	}
	return ""
}

// Region implements storage.RegionAware.  Azure's per-blob
// region is the storage account's region; the SDK doesn't
// expose it without an extra GetAccountInfo call.  Empty
// for now; operators with residency requirements bind the
// account to the region out-of-band.
func (p *Plugin) Region() string { return "" }

func (p *Plugin) assertOpen() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.client == nil {
		return errors.New("azblob: plugin not open")
	}
	return nil
}

// parseAzblobURL extracts the service URL + container +
// optional prefix from an azblob:// URL.
//
//	azblob://acmebackups/prod
//	  → service=https://acmebackups.blob.core.windows.net,
//	    container=prod, prefix=""
//	azblob://acmebackups/prod/db1
//	  → service=...,  container=prod, prefix="db1"
//	azblob://acmebackups.blob.core.usgovcloudapi.net/prod
//	  → service=https://acmebackups.blob.core.usgovcloudapi.net,
//	    container=prod, prefix=""
//
// A dotted host is taken literally; bare account names get
// the public-cloud blob suffix.  Same pattern as azurekv's
// vault-host resolution.
func parseAzblobURL(u *url.URL) (serviceURL, containerName, prefix string, err error) {
	if u.Scheme != "azblob" {
		return "", "", "", fmt.Errorf("azblob: scheme %q is not azblob", u.Scheme)
	}
	host := u.Host
	if host == "" {
		return "", "", "", errors.New("azblob: URL missing account (azblob://<account>/<container>/...)")
	}
	pathSegs := strings.Split(strings.Trim(path.Clean(u.Path), "/"), "/")
	if len(pathSegs) == 0 || pathSegs[0] == "" || pathSegs[0] == "." {
		return "", "", "", errors.New("azblob: URL missing container")
	}
	containerName = pathSegs[0]
	if len(pathSegs) > 1 {
		prefix = strings.Join(pathSegs[1:], "/")
	}
	if strings.Contains(host, ".") {
		serviceURL = "https://" + host
	} else {
		serviceURL = "https://" + host + ".blob.core.windows.net"
	}
	return serviceURL, containerName, prefix, nil
}

// accountNameFromURL extracts the storage account name from
// a service URL.  Used by the SharedKeyCredential builder.
func accountNameFromURL(serviceURL string) string {
	u, err := url.Parse(serviceURL)
	if err != nil {
		return ""
	}
	host := u.Host
	if i := strings.IndexByte(host, '.'); i > 0 {
		return host[:i]
	}
	return host
}

// isNotFound checks the wrapped Azure error code for the
// blob-not-found shape.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return bloberror.HasCode(err, bloberror.BlobNotFound) ||
		bloberror.HasCode(err, bloberror.ResourceNotFound) ||
		bloberror.HasCode(err, bloberror.ContainerNotFound)
}

// isConditionFailed checks for the IfNoneMatch failure that
// our Put/Rename use to emulate IfNotExists.
func isConditionFailed(err error) bool {
	if err == nil {
		return false
	}
	return bloberror.HasCode(err, bloberror.ConditionNotMet) ||
		bloberror.HasCode(err, bloberror.BlobAlreadyExists)
}

// countingReader wraps an io.Reader and tracks bytes read.
// Azure's UploadStream doesn't return a byte count; we
// supply Size on the PutResult by wrapping the input.
type countingReader struct {
	r io.Reader
	n int64
}

// Read implements io.Reader; the byte count is added to the running
// total surfaced as Size on the PutResult.
func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
