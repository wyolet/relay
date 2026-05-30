package s3

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/wyolet/relay/pkg/payload"
)

var _ payload.Reader = (*Reader)(nil)

// Reader is a payload.Reader over the object layout the Sink writes
// (<prefix>/YYYY/MM/DD/<request_id>.json). Get locates a single object by
// request-id suffix. There is no List — the Logs list is served by the log
// store, not the body store.
type Reader struct {
	client *minio.Client
	bucket string
	prefix string
}

// NewReader constructs a Reader against the same bucket/prefix the Sink
// targets. It verifies the bucket is reachable.
func NewReader(ctx context.Context, cfg Config) (*Reader, error) {
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
	return &Reader{client: cl, bucket: cfg.Bucket, prefix: normPrefix(cfg.Prefix)}, nil
}

// Get fetches the object whose key ends with /<requestID>.json. Without a
// timestamp the date partition is unknown, so it scans recursively under the
// prefix and stops at the first match. Returns payload.ErrNotFound if absent.
func (r *Reader) Get(ctx context.Context, requestID string) (payload.Record, error) {
	suffix := "/" + requestID + ".json"
	base := r.prefix
	if base == "" {
		suffix = requestID + ".json" // root-level objects have no leading slash
	}
	for obj := range r.client.ListObjects(ctx, r.bucket, minio.ListObjectsOptions{
		Prefix:    base,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return payload.Record{}, fmt.Errorf("payload/s3: list: %w", obj.Err)
		}
		if strings.HasSuffix(obj.Key, suffix) || obj.Key == suffix {
			return r.fetchObject(ctx, obj.Key)
		}
	}
	return payload.Record{}, payload.ErrNotFound
}

func (r *Reader) fetchObject(ctx context.Context, key string) (payload.Record, error) {
	o, err := r.client.GetObject(ctx, r.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return payload.Record{}, fmt.Errorf("payload/s3: get %q: %w", key, err)
	}
	defer o.Close()
	data, err := io.ReadAll(o)
	if err != nil {
		// A racing delete surfaces here as a read error; treat as absent.
		return payload.Record{}, payload.ErrNotFound
	}
	var rec payload.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return payload.Record{}, fmt.Errorf("payload/s3: decode %q: %w", key, err)
	}
	return rec, nil
}

func normPrefix(p string) string {
	for len(p) > 0 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	return p
}
