// Package s3 implements storage.StoragePlugin against an S3-compatible
// object store. AWS S3, MinIO, Cloudflare R2, and Backblaze B2 all
// honour the same API surface; behavioural differences (path-style
// addressing, endpoint override, lack of Object Lock) are surfaced
// per-backend via Capabilities.
//
// URL format:
//
//	s3://<bucket>/<optional-prefix>?region=us-east-1&endpoint=https://...&path_style=true
//
// Query parameters:
//
//	region        explicit region; otherwise picked up from env/profile
//	endpoint      S3-compatible endpoint (MinIO, R2). Without this we
//	              talk to AWS S3.
//	path_style    "true" forces path-style addressing (bucket in path,
//	              not host). Required for MinIO/localstack with non-DNS
//	              bucket names.
//	storage_class default StorageClass for Put when none is set per-request.
//
// Authentication is delegated to the AWS SDK v2 default credential
// chain (env vars, IRSA, IAM role, profile, SSO). v0.1 does not
// support inline credentials in the URL — operators wanting that
// indirection set AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY in the
// agent's environment.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// DefaultHTTPTimeout bounds an individual S3 request. The AWS SDK
// has built-in retry with exponential backoff for transient errors,
// but the per-attempt timeout is not set out of the box — without
// this, a stalled connection (TCP keepalive's hour-long default;
// half-open after a network partition; backend-side stuck-process)
// can hang well past any operator's RTO budget.
//
// 5 minutes is the upper bound for a single PUT of a large chunk
// (the SPEC tops chunk size at 256 KiB; in practice GET/PUT round-
// trips are sub-second). Operators wanting tighter timeouts pass a
// ctx with their own deadline; this is the floor below which we
// give up and let the SDK's retry layer take over.
const DefaultHTTPTimeout = 5 * time.Minute

func init() {
	storage.Register("s3", func() storage.StoragePlugin { return &Plugin{} })
}

// Plugin is the S3 StoragePlugin. Field set is locked at Open and
// not mutated thereafter, so concurrent Put/Get calls are safe.
type Plugin struct {
	storage.NopBarrier // an S3 PUT is server-side durable on 200-OK

	client       *s3.Client
	bucket       string
	prefix       string // includes a trailing "/" or is empty
	storageClass string
	region       string // resolved AWS region; reported via Region()
}

// Name implements storage.StoragePlugin.
func (p *Plugin) Name() string { return "s3" }

// Region implements storage.RegionAware. Returns the operator-set or
// SDK-resolved region. For S3-compat endpoints with no meaningful
// region the value is whatever the SDK defaulted to ("us-east-1");
// operators relying on residency gating should set the region
// explicitly via ?region=... in the URL.
func (p *Plugin) Region() string { return p.region }

// Open implements storage.StoragePlugin. Parses URL + query, builds
// an aws.Config via the default credential chain, and constructs an
// s3.Client. We do NOT verify bucket existence here — that's a
// HEAD-bucket round-trip every CLI invocation would pay for nothing.
// First failed Put / Get / List surfaces the error with the same
// fidelity.
func (p *Plugin) Open(ctx context.Context, cfg storage.StorageConfig) error {
	if cfg.URL == nil {
		return errors.New("s3: nil URL")
	}
	bucket, prefix, err := parseS3URL(cfg.URL)
	if err != nil {
		return err
	}

	q := cfg.URL.Query()
	region := q.Get("region")
	endpoint := q.Get("endpoint")
	pathStyle := q.Get("path_style") == "true"
	storageClass := q.Get("storage_class")

	// Per-request HTTP timeout. The SDK's default HTTPClient has
	// no overall timeout; combined with TCP keepalive's
	// hour-long default this can hang an agent indefinitely on
	// a stalled connection. Set DefaultHTTPTimeout as the upper
	// bound; operators wanting tighter limits pass a ctx with
	// a deadline.
	//
	// awshttp.NewBuildableClient (rather than a plain
	// *http.Client) is what lets the SDK extend the transport
	// with custom RootCAs from AWS_CA_BUNDLE / AWS_CA_BUNDLE_DATA.
	// A bare *http.Client doesn't satisfy the SDK's
	// WithTransportOptions interface, and config.LoadDefaultConfig
	// fails with "unable to add custom RootCAs HTTPClient" the
	// moment AWS_CA_BUNDLE is set in the environment — exactly
	// what self-signed-cert testing surfaced.
	httpClient := awshttp.NewBuildableClient().WithTimeout(DefaultHTTPTimeout)

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		func(o *awsconfig.LoadOptions) error {
			if region != "" {
				o.Region = region
			}
			o.HTTPClient = httpClient
			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("s3: load aws config: %w", err)
	}
	if awsCfg.Region == "" {
		// AWS S3 needs a region; S3-compat endpoints often don't
		// care but we still need SOMETHING for the SDK's signer.
		// us-east-1 is the conventional default for legacy buckets.
		awsCfg.Region = "us-east-1"
	}

	// Checksum behaviour (issue #86).  aws-sdk-go-v2 v1.36+
	// defaults RequestChecksumCalculation to WhenSupported, which
	// adds an `x-amz-sdk-checksum-algorithm: CRC32` header and a
	// `x-amz-checksum-crc32: ...` body header to every PutObject.
	// Ceph-RGW-based S3-compatible services (Hetzner Object
	// Storage, some MinIO configurations, Backblaze B2 strict
	// mode) reject those headers with `400 InvalidRequest`,
	// which the reporter hit on the first chunk PUT.  Same story
	// for ResponseChecksumValidation: WhenSupported makes the
	// SDK log a noisy `WARN Response has no supported checksum`
	// per round-trip when the upstream omits the algorithm
	// header the SDK would validate against.
	//
	// We default both knobs to `WhenRequired` so the SDK only
	// adds / validates checksums when the operation explicitly
	// needs them (object-lock-protected uploads still get their
	// SHA-256 because RetainUntilDate uploads require it).  Note
	// that pg_hardstorage already content-addresses every chunk
	// by SHA-256 in the CAS layer — the SDK's CRC32 is a
	// duplicative defence layer that costs us compatibility with
	// half the S3-compat market for no real integrity gain.
	//
	// Operators who specifically want the v1.36+ default-on
	// behaviour (e.g. real AWS S3 with strict integrity checks)
	// can opt back in via `?checksum=when_supported` on the
	// repo URL.
	switch q.Get("checksum") {
	case "", "when_required", "required":
		awsCfg.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		awsCfg.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	case "when_supported", "supported":
		awsCfg.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenSupported
		awsCfg.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenSupported
	default:
		return fmt.Errorf("s3: unknown checksum=%q; want when_required (default) or when_supported",
			q.Get("checksum"))
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		o.UsePathStyle = pathStyle || endpoint != ""
	})

	p.client = client
	p.bucket = bucket
	p.prefix = prefix
	p.storageClass = storageClass
	p.region = awsCfg.Region
	return nil
}

// parseS3URL pulls (bucket, prefix) from `s3://bucket/<prefix>`. The
// prefix is normalised to either empty or a string ending in `/`,
// so concatenation with a key always produces a valid object path.
func parseS3URL(u *url.URL) (bucket, prefix string, err error) {
	if u.Scheme != "s3" {
		return "", "", fmt.Errorf("s3: scheme %q (want s3)", u.Scheme)
	}
	bucket = u.Host
	if bucket == "" {
		return "", "", errors.New("s3: URL has no bucket (use s3://<bucket>/<prefix>)")
	}
	prefix = strings.TrimPrefix(u.Path, "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return bucket, prefix, nil
}

// Close implements storage.StoragePlugin. The S3 client has no
// long-lived resources to release, so this is a no-op.
func (p *Plugin) Close() error { return nil }

// fullKey prepends the configured prefix to key.
func (p *Plugin) fullKey(key string) string {
	return p.prefix + key
}

// Capabilities implements storage.StoragePlugin. WORM is reported as
// available because S3 supports Object Lock; Capabilities() doesn't
// know if a specific bucket has Object Lock enabled — that surfaces
// at SetRetention time as a 400 from the API.
func (p *Plugin) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		WORM:                   true,
		ConditionalPut:         true,
		Multipart:              true,
		ServerSideEncryption:   true,
		CrossRegionReplicate:   true,
		StorageClassSelectable: true,
		InlineDurable:          true, // a 200-OK PUT is durable
	}
}

// Put implements storage.StoragePlugin.
//
// Conditional writes use S3's `If-None-Match: *` header — atomic at
// the service level. Storage class falls back to the URL-level
// default when PutOptions.StorageClass is empty.
//
// Top-of-method ctx.Err() check matches the fs plugin's posture.
// The AWS SDK respects ctx through every call, so an in-flight
// request IS interruptible — but an already-cancelled ctx is
// cheaper to surface here than to reach the SDK's resolve-endpoint
// + sign-request + DNS work that happens before the HTTP call.
func (p *Plugin) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if err := ctx.Err(); err != nil {
		return storage.PutResult{}, err
	}
	in := &s3.PutObjectInput{
		Bucket: aws.String(p.bucket),
		Key:    aws.String(p.fullKey(key)),
		Body:   r,
	}
	if opts.IfNotExists {
		in.IfNoneMatch = aws.String("*")
	}
	if opts.ContentLength > 0 {
		in.ContentLength = aws.Int64(opts.ContentLength)
	}
	storageClass := opts.StorageClass
	if storageClass == "" {
		storageClass = p.storageClass
	}
	if storageClass != "" {
		in.StorageClass = s3types.StorageClass(storageClass)
	}
	if len(opts.Metadata) > 0 {
		in.Metadata = opts.Metadata
	}
	if !opts.RetainUntil.IsZero() {
		in.ObjectLockRetainUntilDate = aws.Time(opts.RetainUntil)
		// PutOptions.RetentionMode selects compliance vs governance.
		// Empty defaults to compliance — the regulatory-grade
		// posture (even root credentials cannot delete before the
		// deadline). Operators wanting the more permissive
		// governance mode (BypassGovernance permission can delete)
		// set it explicitly.
		switch opts.RetentionMode {
		case storage.WORMGovernance:
			in.ObjectLockMode = s3types.ObjectLockModeGovernance
		default:
			in.ObjectLockMode = s3types.ObjectLockModeCompliance
		}
	}

	// Capture body size BEFORE the Put — the AWS SDK needs
	// the underlying reader to remain a Seeker (for SigV4
	// payload checksum without TLS), so we can't wrap.
	// Three sources of truth, in priority:
	//   1. opts.ContentLength when the caller knows
	//   2. r.Size() when r is bytes.Reader / strings.Reader
	//      / similar (the contract harness uses these)
	//   3. r.Stat().Size() when r is *os.File
	// Anything else stays 0 — a pure streaming source
	// without metadata can't be measured pre-flight, and
	// faking it would be worse than honest zero.
	bodySize := opts.ContentLength
	if bodySize == 0 {
		if sized, ok := r.(interface{ Size() int64 }); ok {
			bodySize = sized.Size()
		} else if statf, ok := r.(interface{ Stat() (os.FileInfo, error) }); ok {
			if fi, err := statf.Stat(); err == nil {
				bodySize = fi.Size()
			}
		}
	}

	out, err := p.client.PutObject(ctx, in)
	if err != nil {
		if isPreconditionFailed(err) {
			return storage.PutResult{}, fmt.Errorf("%w: %s", storage.ErrAlreadyExists, key)
		}
		return storage.PutResult{}, fmt.Errorf("s3: put %q: %w", key, err)
	}

	res := storage.PutResult{Key: key, Size: bodySize}
	if out.ETag != nil {
		res.ETag = *out.ETag
	}
	if out.VersionId != nil {
		res.VersionID = *out.VersionId
	}
	return res, nil
}

// Get implements storage.StoragePlugin.
func (p *Plugin) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := p.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(p.bucket),
		Key:    aws.String(p.fullKey(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", storage.ErrNotFound, key)
		}
		return nil, fmt.Errorf("s3: get %q: %w", key, err)
	}
	return out.Body, nil
}

// Stat implements storage.StoragePlugin.
func (p *Plugin) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}
	out, err := p.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(p.bucket),
		Key:    aws.String(p.fullKey(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return storage.ObjectInfo{}, fmt.Errorf("%w: %s", storage.ErrNotFound, key)
		}
		return storage.ObjectInfo{}, fmt.Errorf("s3: head %q: %w", key, err)
	}
	return objectInfoFrom(key, out), nil
}

// Delete implements storage.StoragePlugin. Idempotent.
func (p *Plugin) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := p.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(p.bucket),
		Key:    aws.String(p.fullKey(key)),
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("s3: delete %q: %w", key, err)
	}
	return nil
}

// List implements storage.StoragePlugin. Paginates ListObjectsV2
// transparently.
func (p *Plugin) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	full := p.fullKey(prefix)
	return func(yield func(storage.ObjectInfo, error) bool) {
		// Pre-walk ctx check; per-entry checks inside the loop
		// cooperate with the paginator's own ctx-aware NextPage.
		if err := ctx.Err(); err != nil {
			yield(storage.ObjectInfo{}, err)
			return
		}
		paginator := s3.NewListObjectsV2Paginator(p.client, &s3.ListObjectsV2Input{
			Bucket: aws.String(p.bucket),
			Prefix: aws.String(full),
		})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				yield(storage.ObjectInfo{}, fmt.Errorf("s3: list %q: %w", prefix, err))
				return
			}
			for _, obj := range page.Contents {
				if obj.Key == nil {
					continue
				}
				// Strip the configured prefix from the returned key
				// so downstream code sees the same shape it would
				// see from a file:// repo.
				rel := strings.TrimPrefix(*obj.Key, p.prefix)
				info := storage.ObjectInfo{
					Key: rel,
				}
				if obj.Size != nil {
					info.Size = *obj.Size
				}
				if obj.LastModified != nil {
					info.ModTime = *obj.LastModified
				}
				if obj.ETag != nil {
					info.ETag = *obj.ETag
				}
				if obj.StorageClass != "" {
					info.StorageClass = string(obj.StorageClass)
				}
				if !yield(info, nil) {
					return
				}
			}
		}
	}
}

// RenameIfNotExists implements storage.StoragePlugin via copy +
// delete. Atomicity is enforced on the destination side via
// `If-None-Match: *` on the CopyObject. The source is then deleted;
// a crash between copy and delete leaves a stale src object that
// the next backup-orchestrator commit's tmp-cleanup logic reaps,
// or that GC sweeps.
//
// Race window: If two concurrent renames target the same dst, the
// second's CopyObject fails with PreconditionFailed and the caller
// gets ErrAlreadyExists — the same semantic the fs plugin has via
// link(2). Net effect: same correctness, slightly larger window
// (3 round-trips instead of 1 syscall).
//
// Source-delete error handling: a Delete failure after a successful
// Copy is REPORTED to the caller (wrapped, with the dst already
// committed to the new key). The orphaned src object will be reaped
// by the next tmp-cleanup or GC pass, but the operator needs the
// signal — silently swallowing the error means costs accumulate
// invisibly until someone notices the bucket grew. NotFound on the
// delete is tolerated (idempotent re-rename, or a concurrent sweep).
func (p *Plugin) RenameIfNotExists(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Defence in depth: HEAD dst FIRST and refuse if it
	// exists.  Without this we relied on
	// `IfNoneMatch: "*"` on CopyObject; AWS S3 honours that
	// directive but several S3-compat backends — including
	// MinIO as of RELEASE.2025-01-20T14-49-07Z — silently
	// IGNORE it on CopyObject and proceed with the
	// overwrite.  The contract harness caught this:
	// dst-present + RenameIfNotExists succeeded and
	// clobbered the existing object.  For
	// manifest-commit semantics that's a data-loss class
	// bug — two backups racing the same backup ID would
	// silently overwrite each other.
	//
	// The HEAD-then-CopyObject pattern has an unavoidable
	// race window between the HEAD and the COPY (a
	// concurrent writer can land in between), but the
	// agent serialises commits per backup ID via the audit
	// chain anyway, so the race window doesn't matter in
	// practice.  The IfNoneMatch directive remains as a
	// SECOND line of defence on backends that honour it.
	if _, err := p.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(p.bucket),
		Key:    aws.String(p.fullKey(dst)),
	}); err == nil {
		return fmt.Errorf("%w: %s", storage.ErrAlreadyExists, dst)
	} else if !isNotFound(err) {
		return fmt.Errorf("s3: rename precheck head %q: %w", dst, err)
	}

	_, err := p.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:      aws.String(p.bucket),
		Key:         aws.String(p.fullKey(dst)),
		CopySource:  aws.String(url.PathEscape(p.bucket + "/" + p.fullKey(src))),
		IfNoneMatch: aws.String("*"),
	})
	if err != nil {
		if isPreconditionFailed(err) {
			return fmt.Errorf("%w: %s", storage.ErrAlreadyExists, dst)
		}
		if isNotFound(err) {
			return fmt.Errorf("%w: src %s", storage.ErrNotFound, src)
		}
		return fmt.Errorf("s3: copy %q -> %q: %w", src, dst, err)
	}
	// Delete src. Tolerate NotFound (idempotency); surface anything
	// else — the dst commit already succeeded, but the operator still
	// needs to know an orphan was left behind.
	if _, err := p.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(p.bucket),
		Key:    aws.String(p.fullKey(src)),
	}); err != nil && !isNotFound(err) {
		return fmt.Errorf("s3: rename %q -> %q: copy succeeded but unlink src failed: %w",
			src, dst, err)
	}
	return nil
}

// SetRetention implements storage.StoragePlugin. Maps the WORMMode
// to S3 Object Lock's GOVERNANCE / COMPLIANCE.
func (p *Plugin) SetRetention(ctx context.Context, key string, until time.Time, mode storage.WORMMode) error {
	var lockMode s3types.ObjectLockRetentionMode
	switch mode {
	case storage.WORMNone:
		return nil
	case storage.WORMGovernance:
		lockMode = s3types.ObjectLockRetentionModeGovernance
	case storage.WORMCompliance:
		lockMode = s3types.ObjectLockRetentionModeCompliance
	default:
		return fmt.Errorf("s3: unknown WORM mode %q", mode)
	}
	_, err := p.client.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{
		Bucket: aws.String(p.bucket),
		Key:    aws.String(p.fullKey(key)),
		Retention: &s3types.ObjectLockRetention{
			Mode:            lockMode,
			RetainUntilDate: aws.Time(until),
		},
	})
	if err != nil {
		return fmt.Errorf("s3: set retention %q: %w", key, err)
	}
	return nil
}

// objectInfoFrom builds a storage.ObjectInfo from a HeadObject result.
func objectInfoFrom(key string, out *s3.HeadObjectOutput) storage.ObjectInfo {
	info := storage.ObjectInfo{Key: key}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}
	if out.LastModified != nil {
		info.ModTime = *out.LastModified
	}
	if out.ETag != nil {
		info.ETag = *out.ETag
	}
	if out.StorageClass != "" {
		info.StorageClass = string(out.StorageClass)
	}
	if out.Metadata != nil {
		info.Metadata = out.Metadata
	}
	return info
}

// isNotFound reports whether err is an S3 NoSuchKey / NotFound /
// 404. The SDK exposes typed errors but the structured-error API
// varies per operation; the smithy.APIError code is the most
// portable path.
func isNotFound(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	var nsk *s3types.NoSuchKey
	return errors.As(err, &nsk)
}

// isPreconditionFailed reports whether err corresponds to a
// PreconditionFailed (412) — what S3 returns when If-None-Match
// rejects an overwrite.
func isPreconditionFailed(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "PreconditionFailed", "412":
			return true
		}
	}
	return false
}
