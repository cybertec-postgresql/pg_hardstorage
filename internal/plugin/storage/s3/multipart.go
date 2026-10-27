package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// multipartThreshold is the object size above which Put uploads in parts.
// A single PutObject is capped at 5 GiB by S3, and needs a seekable body;
// a manifest for a database with billions of chunks is hundreds of GB and
// arrives as a stream. Everything below the threshold keeps the single
// conditional PUT it always used.
var multipartThreshold int64 = 64 << 20

var minPartSize int64 = 64 << 20

const (
	maxParts     int64 = 9000 // S3 allows 10,000; leave headroom
	maxObjectLen int64 = 5 << 40
)

// partSizeFor picks a part size that keeps size within maxParts parts.
func partSizeFor(size int64) int64 {
	ps := (size + maxParts - 1) / maxParts
	if ps < minPartSize {
		ps = minPartSize
	}
	return ps
}

// putMultipart streams exactly size bytes from r as a multipart upload.
// IfNotExists is enforced on CompleteMultipartUpload (S3's conditional
// write for multipart), so the exclusive-commit contract of Put holds;
// object-lock retention is requested at CreateMultipartUpload. A failed
// upload is aborted so no orphan parts keep accruing storage.
func (p *Plugin) putMultipart(ctx context.Context, key string, r io.Reader, size int64, opts storage.PutOptions, in *s3.PutObjectInput) (storage.PutResult, error) {
	if size > maxObjectLen {
		return storage.PutResult{}, fmt.Errorf("s3: put %q: %d bytes exceeds the 5 TiB S3 object limit", key, size)
	}
	create, err := p.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:                    in.Bucket,
		Key:                       in.Key,
		StorageClass:              in.StorageClass,
		Metadata:                  in.Metadata,
		ObjectLockMode:            in.ObjectLockMode,
		ObjectLockRetainUntilDate: in.ObjectLockRetainUntilDate,
	})
	if err != nil {
		return storage.PutResult{}, fmt.Errorf("s3: put %q: create multipart: %w", key, err)
	}
	uploadID := create.UploadId
	abort := func() {
		cctx, cancel := storage.CleanupContext(ctx)
		defer cancel()
		_, _ = p.client.AbortMultipartUpload(cctx, &s3.AbortMultipartUploadInput{
			Bucket: in.Bucket, Key: in.Key, UploadId: uploadID,
		})
	}

	partSize := partSizeFor(size)
	buf := make([]byte, partSize)
	var parts []s3types.CompletedPart
	var sent int64
	for n := int32(1); sent < size; n++ {
		want := partSize
		if size-sent < want {
			want = size - sent
		}
		if _, err := io.ReadFull(r, buf[:want]); err != nil {
			abort()
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return storage.PutResult{}, fmt.Errorf("s3: put %q: body ended after %d of %d bytes", key, sent, size)
			}
			return storage.PutResult{}, fmt.Errorf("s3: put %q: read part %d: %w", key, n, err)
		}
		up, err := p.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: in.Bucket, Key: in.Key, UploadId: uploadID,
			PartNumber:    aws.Int32(n),
			Body:          bytes.NewReader(buf[:want]),
			ContentLength: aws.Int64(want),
		})
		if err != nil {
			abort()
			return storage.PutResult{}, fmt.Errorf("s3: put %q: upload part %d: %w", key, n, err)
		}
		parts = append(parts, s3types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(n)})
		sent += want
	}

	done := &s3.CompleteMultipartUploadInput{
		Bucket: in.Bucket, Key: in.Key, UploadId: uploadID,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	}
	var callOpts []func(*s3.Options)
	if opts.IfNotExists {
		done.IfNoneMatch = aws.String("*")
		callOpts = append(callOpts, retryConditionalConflict)
	}
	out, err := p.client.CompleteMultipartUpload(ctx, done, callOpts...)
	if err != nil {
		abort()
		if isPreconditionFailed(err) {
			return storage.PutResult{}, fmt.Errorf("%w: %s", storage.ErrAlreadyExists, key)
		}
		return storage.PutResult{}, fmt.Errorf("s3: put %q: complete multipart: %w", key, err)
	}
	res := storage.PutResult{Key: key, Size: size}
	if out.ETag != nil {
		res.ETag = *out.ETag
	}
	if out.VersionId != nil {
		res.VersionID = *out.VersionId
	}
	return res, nil
}
