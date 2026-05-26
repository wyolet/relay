// Package s3 is the object-store backend for payload logging: one JSON
// object per Record. S3-compatible via minio-go, so it targets AWS S3,
// MinIO, and other compatible stores with the same client. Implements
// payload.Sink.
//
// This package pulls in the minio-go SDK. It is excluded from the relay
// "minimal" build (see cmd/relay's build-tagged sink seam), so a minimal
// binary/image carries none of these deps.
package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/wyolet/relay/pkg/payload"
)

var _ payload.Sink = (*Sink)(nil)

// Config holds the object-store connection + placement settings.
type Config struct {
	Endpoint  string // host:port (no scheme), e.g. "s3.amazonaws.com" or "minio.dev-stack:9000"
	Bucket    string
	Region    string // optional; AWS uses it, MinIO ignores it
	AccessKey string
	SecretKey string
	Prefix    string // key prefix, e.g. "payloads"; trailing slash optional
	UseSSL    bool
}

// Sink writes each Record as an object at
// <prefix>/<YYYY>/<MM>/<DD>/<request_id>.json. PutObject is synchronous
// per call; the emitter's single drain goroutine serializes writes and
// drop-on-full upstream protects the hot path.
type Sink struct {
	client *minio.Client
	bucket string
	prefix string
}

// New constructs the sink and verifies the bucket is reachable.
func New(ctx context.Context, cfg Config) (*Sink, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("payload/s3: bucket is required")
	}
	cl, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("payload/s3: client: %w", err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ok, err := cl.BucketExists(checkCtx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("payload/s3: bucket check %q: %w", cfg.Bucket, err)
	}
	if !ok {
		return nil, fmt.Errorf("payload/s3: bucket %q does not exist", cfg.Bucket)
	}
	return &Sink{client: cl, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

func (s *Sink) Write(r payload.Record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("payload/s3: marshal: %w", err)
	}
	key := objectKey(s.prefix, r.Timestamp, r.RequestID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/json"})
	if err != nil {
		return fmt.Errorf("payload/s3: put %q: %w", key, err)
	}
	return nil
}

// objectKey lays records out date-partitioned for cheap prefix listing:
// <prefix>/YYYY/MM/DD/<request_id>.json. A zero ts falls back to now so a
// missing timestamp never collapses the partition layout.
func objectKey(prefix string, ts time.Time, requestID string) string {
	if ts.IsZero() {
		ts = time.Now()
	}
	ts = ts.UTC()
	base := fmt.Sprintf("%04d/%02d/%02d/%s.json", ts.Year(), ts.Month(), ts.Day(), requestID)
	if prefix == "" {
		return base
	}
	if prefix[len(prefix)-1] == '/' {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + "/" + base
}
