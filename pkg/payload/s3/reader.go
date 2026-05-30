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

// Reader is a payload.Reader over the date-partitioned object layout the
// Sink writes (<prefix>/YYYY/MM/DD/<request_id>.json). List narrows the
// scan to the day partitions the time window touches; Get locates a single
// object by request-id suffix. Each matching object is fetched and decoded
// in Go — fine for the interactive Logs view at dogfood scale.
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

// List enumerates the day partitions the query window covers, fetches each
// object, filters in Go, sorts newest-first, and strips bodies.
func (r *Reader) List(ctx context.Context, q payload.Query) ([]payload.Record, error) {
	var all []payload.Record
	for _, p := range r.scanPrefixes(q) {
		recs, err := r.fetchUnder(ctx, p)
		if err != nil {
			return nil, err
		}
		all = append(all, recs...)
	}
	matched := payload.SortAndLimit(payload.FilterRecords(all, q), q.Limit)
	for i := range matched {
		matched[i] = payload.StripBody(matched[i])
	}
	return matched, nil
}

// Get fetches the single object whose key ends with /<requestID>.json. Without
// a timestamp the partition is unknown, so it scans recursively under the
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

// scanPrefixes returns the set of key prefixes to list for the query window.
// A bounded window narrows to per-day prefixes; an unbounded window falls
// back to the root prefix (full recursive scan).
func (r *Reader) scanPrefixes(q payload.Query) []string {
	from, to := windowBounds(q)
	if from.IsZero() {
		return []string{r.prefix} // unbounded — scan everything
	}
	var out []string
	for d := from.UTC().Truncate(24 * time.Hour); !d.After(to); d = d.Add(24 * time.Hour) {
		day := fmt.Sprintf("%04d/%02d/%02d/", d.Year(), d.Month(), d.Day())
		if r.prefix == "" {
			out = append(out, day)
		} else {
			out = append(out, r.prefix+"/"+day)
		}
	}
	return out
}

// fetchUnder lists + decodes every object under one prefix.
func (r *Reader) fetchUnder(ctx context.Context, prefix string) ([]payload.Record, error) {
	var out []payload.Record
	for obj := range r.client.ListObjects(ctx, r.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("payload/s3: list %q: %w", prefix, obj.Err)
		}
		if !strings.HasSuffix(obj.Key, ".json") {
			continue
		}
		rec, err := r.fetchObject(ctx, obj.Key)
		if err == payload.ErrNotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
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

// windowBounds resolves the query's effective [from, to]. A zero from means
// unbounded-below; to defaults to now when a lower bound exists.
func windowBounds(q payload.Query) (from, to time.Time) {
	switch {
	case !q.From.IsZero():
		from = q.From
	case q.Since > 0:
		from = time.Now().Add(-q.Since)
	}
	to = q.To
	if to.IsZero() {
		to = time.Now()
	}
	return from, to
}

func normPrefix(p string) string {
	for len(p) > 0 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	return p
}
