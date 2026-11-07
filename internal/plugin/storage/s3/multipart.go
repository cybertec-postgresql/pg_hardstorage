package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

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

// Parts upload in parallel: a serial loop is bound by parts × round trip,
// which on a multi-hundred-GB manifest is thousands of sequential
// requests. Every in-flight part holds its own buffer, so the fan-out is
// bounded twice — by maxPartConcurrency and by partBufferBudget of
// memory. At the default 64 MiB part that is 4 parts (256 MiB); once the
// part size has to grow to stay under maxParts, concurrency drops until
// a single part is in flight.
var (
	maxPartConcurrency       = 4
	partBufferBudget   int64 = 256 << 20
)

// partConcurrencyFor is the number of parts uploaded at once.
func partConcurrencyFor(partSize int64, nParts int) int {
	c := int(partBufferBudget / partSize)
	c = min(c, maxPartConcurrency, nParts)
	return max(c, 1)
}

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
	// An endpoint known to reject If-None-Match gets the read first,
	// not after every part has gone up; the completion reads again
	// for the window in between.
	if opts.IfNotExists && p.noCondPut.Load() {
		if err := p.refuseIfPresent(ctx, key); err != nil {
			return storage.PutResult{}, err
		}
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
	nParts := int((size + partSize - 1) / partSize)
	conc := partConcurrencyFor(partSize, nParts)
	// The body is a stream, so it is read here, in order, one part at a
	// time; only the uploads overlap. free holds one token per upload
	// slot — a buffer once that slot has been used — so a part is read
	// only when there is a slot to send it from.
	free := make(chan []byte, conc)
	for i := 0; i < conc; i++ {
		free <- nil
	}
	uctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg      sync.WaitGroup
		failed  sync.Once
		partErr error
	)
	parts := make([]s3types.CompletedPart, nParts)
	var readErr error
	var sent int64
read:
	for n := int32(1); sent < size; n++ {
		var buf []byte
		select {
		case buf = <-free:
		case <-uctx.Done():
			break read
		}
		want := min(partSize, size-sent)
		if buf == nil {
			buf = make([]byte, partSize)
		}
		if _, err := io.ReadFull(r, buf[:want]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				readErr = fmt.Errorf("s3: put %q: body ended after %d of %d bytes", key, sent, size)
			} else {
				readErr = fmt.Errorf("s3: put %q: read part %d: %w", key, n, err)
			}
			cancel()
			break
		}
		wg.Add(1)
		go func(n int32, buf []byte, want int64) {
			defer wg.Done()
			defer func() { free <- buf }()
			up, err := p.client.UploadPart(uctx, &s3.UploadPartInput{
				Bucket: in.Bucket, Key: in.Key, UploadId: uploadID,
				PartNumber:    aws.Int32(n),
				Body:          bytes.NewReader(buf[:want]),
				ContentLength: aws.Int64(want),
			})
			if err != nil {
				// The first failure is the one reported; it cancels
				// the siblings, whose "context canceled" is noise.
				failed.Do(func() {
					partErr = fmt.Errorf("s3: put %q: upload part %d: %w", key, n, err)
					cancel()
				})
				return
			}
			parts[n-1] = s3types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(n)}
		}(n, buf, want)
		sent += want
	}
	// Abort only once nothing is in flight: a part landing after the
	// abort would be an orphan nobody cleans up.
	wg.Wait()
	switch {
	case readErr != nil:
		abort()
		return storage.PutResult{}, readErr
	case partErr != nil:
		abort()
		return storage.PutResult{}, partErr
	case sent < size:
		abort()
		return storage.PutResult{}, fmt.Errorf("s3: put %q: %w", key, context.Cause(ctx))
	}

	done := &s3.CompleteMultipartUploadInput{
		Bucket: in.Bucket, Key: in.Key, UploadId: uploadID,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	}
	var callOpts []func(*s3.Options)
	emulate := opts.IfNotExists && p.noCondPut.Load()
	if opts.IfNotExists && !emulate {
		done.IfNoneMatch = aws.String("*")
		callOpts = append(callOpts, retryConditionalConflict)
	}
	complete := func() (*s3.CompleteMultipartUploadOutput, error) {
		if emulate {
			// The endpoint rejects If-None-Match (see Put): the read
			// before the write is the only exclusion there is.
			if err := p.refuseIfPresent(ctx, key); err != nil {
				return nil, err
			}
		}
		return p.client.CompleteMultipartUpload(ctx, done, callOpts...)
	}
	out, err := complete()
	if err != nil && opts.IfNotExists && !emulate && isNotImplemented(err) {
		if cerr := p.conditionalPutRefused(key, err); cerr != nil {
			abort()
			return storage.PutResult{}, cerr
		}
		// The parts are still there; only the completion was refused.
		emulate, done.IfNoneMatch, callOpts = true, nil, nil
		out, err = complete()
	}
	if err != nil {
		abort()
		if errors.Is(err, storage.ErrAlreadyExists) {
			return storage.PutResult{}, err
		}
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
