package s3

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/payload"
)

// fakeS3 serves the four calls the backend makes against one bucket named "bucket": HEAD bucket, PUT object, list objects and GET object.
type fakeS3 struct {
	mu           sync.Mutex
	objects      map[string][]byte
	contentTypes map[string]string
}

func newFakeS3(t *testing.T) (*fakeS3, Config) {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}, contentTypes: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, Config{Endpoint: strings.TrimPrefix(srv.URL, "http://"), Bucket: "bucket", Region: "us-east-1", AccessKey: "k", SecretKey: "s"}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != "bucket" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodHead && key == "":
	case r.Method == http.MethodPut:
		body, err := readSignedBody(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.objects[key], f.contentTypes[key] = body, r.Header.Get("Content-Type")
		w.Header().Set("ETag", `"etag"`)
	case r.Method == http.MethodGet && key == "":
		type contents struct {
			Key          string
			Size         int
			LastModified string
			ETag         string
		}
		var list struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Contents []contents
		}
		for k, body := range f.objects {
			if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
				list.Contents = append(list.Contents, contents{k, len(body), "2026-01-01T00:00:00.000Z", `"etag"`})
			}
		}
		sort.Slice(list.Contents, func(i, j int) bool { return list.Contents[i].Key < list.Contents[j].Key })
		_ = xml.NewEncoder(w).Encode(list)
	case r.Method == http.MethodGet:
		body, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Last-Modified", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat))
		w.Header().Set("ETag", `"etag"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// readSignedBody returns a PUT's payload, unwrapping the aws-chunked framing the client signs plain-HTTP uploads with.
func readSignedBody(r *http.Request) ([]byte, error) {
	if !strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		return io.ReadAll(r.Body)
	}
	br := bufio.NewReader(r.Body)
	var out []byte
	for {
		header, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("chunk header: %w", err)
		}
		size, err := strconv.ParseInt(strings.TrimSpace(strings.SplitN(header, ";", 2)[0]), 16, 64)
		if err != nil {
			return nil, fmt.Errorf("chunk size in %q: %w", header, err)
		}
		if size == 0 {
			return out, nil
		}
		chunk := make([]byte, size+2) // data and its CRLF
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, fmt.Errorf("chunk data: %w", err)
		}
		out = append(out, chunk[:size]...)
	}
}

func TestObjectKey(t *testing.T) {
	ts := time.Date(2026, 5, 26, 23, 30, 0, 0, time.FixedZone("UTC-3", -3*3600)) // the 27th in UTC
	for _, tc := range []struct{ prefix, want string }{
		{"payloads", "payloads/2026/05/27/req-1.json"},
		{"payloads/", "payloads/2026/05/27/req-1.json"},
		{"a/b", "a/b/2026/05/27/req-1.json"},
		{"", "2026/05/27/req-1.json"},
	} {
		if got := objectKey(tc.prefix, ts, "req-1"); got != tc.want {
			t.Errorf("objectKey(%q) = %q, want %q", tc.prefix, got, tc.want)
		}
	}
	// A record without a timestamp is filed under the day it is written.
	before := time.Now().UTC().Format("2006/01/02")
	got := objectKey("p", time.Time{}, "req-1")
	after := time.Now().UTC().Format("2006/01/02")
	if got != "p/"+before+"/req-1.json" && got != "p/"+after+"/req-1.json" {
		t.Errorf("objectKey with a zero timestamp = %q, want today's date (%s)", got, after)
	}
}

func TestSinkWriteThenReaderGet(t *testing.T) {
	fake, cfg := newFakeS3(t)
	cfg.Prefix = "payloads/"
	ctx := context.Background()
	sink, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reader, err := NewReader(ctx, cfg)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	rec := payload.Record{
		RequestID: "req-1", Timestamp: time.Date(2026, 5, 26, 14, 30, 0, 0, time.UTC),
		ProjectID: "proj", PrincipalID: "sa-1", RelayKeyHash: "hash",
		RequestBody: []byte(`{"in":"<1>"}`), ResponseBody: []byte("not json \x00\xff"), ResponseTruncated: true,
	}
	other := payload.Record{RequestID: "other-req-1", Timestamp: rec.Timestamp, RequestBody: []byte("other")}
	for _, r := range []payload.Record{other, rec} {
		if err := sink.Write(r); err != nil {
			t.Fatalf("Write(%s): %v", r.RequestID, err)
		}
	}

	const key = "payloads/2026/05/26/req-1.json"
	if _, ok := fake.objects[key]; !ok || fake.contentTypes[key] != "application/json" || len(fake.objects) != 2 {
		t.Fatalf("stored objects = %v (content types %v), want %s as application/json beside one other", keys(fake.objects), fake.contentTypes, key)
	}
	for _, want := range []payload.Record{rec, other} {
		got, err := reader.Get(ctx, want.RequestID)
		if err != nil {
			t.Fatalf("Get(%s): %v", want.RequestID, err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Get(%s) (-written +read):\n%s", want.RequestID, diff)
		}
	}
	// An id is matched as a whole path segment, never as the tail of another id.
	for _, id := range []string{"missing", "1", "eq-1", ""} {
		if _, err := reader.Get(ctx, id); !errors.Is(err, payload.ErrNotFound) {
			t.Errorf("Get(%q): err = %v, want ErrNotFound", id, err)
		}
	}

	fake.objects["payloads/2026/05/26/broken.json"] = []byte("{not json")
	if _, err := reader.Get(ctx, "broken"); err == nil || !strings.HasPrefix(err.Error(), `payload/s3: decode "payloads/2026/05/26/broken.json": `) {
		t.Errorf("Get of an undecodable object: err = %v, want a decode error", err)
	}
}

func TestReaderGetWithoutPrefixMatchesWholeIDs(t *testing.T) {
	_, cfg := newFakeS3(t)
	ctx := context.Background()
	sink, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	other := payload.Record{RequestID: "other-req-1", Timestamp: time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC), RequestBody: []byte("other")}
	if err := sink.Write(other); err != nil {
		t.Fatal(err)
	}
	if got, err := reader.Get(ctx, "other-req-1"); err != nil || got.RequestID != "other-req-1" {
		t.Fatalf("Get(other-req-1) = %+v, %v", got, err)
	}
	if got, err := reader.Get(ctx, "req-1"); !errors.Is(err, payload.ErrNotFound) {
		t.Errorf("Get(req-1) = record %q, %v; want ErrNotFound", got.RequestID, err)
	}
}

func TestNewRejectsAnUnusableBucket(t *testing.T) {
	_, cfg := newFakeS3(t)
	ctx := context.Background()
	for _, tc := range []struct{ bucket, want string }{
		{"", "payload/s3: bucket is required"},
		{"absent", `payload/s3: bucket "absent" does not exist`},
	} {
		cfg.Bucket = tc.bucket
		if _, err := New(ctx, cfg); err == nil || err.Error() != tc.want {
			t.Errorf("New(bucket %q): err = %v, want %q", tc.bucket, err, tc.want)
		}
		if _, err := NewReader(ctx, cfg); err == nil || err.Error() != tc.want {
			t.Errorf("NewReader(bucket %q): err = %v, want %q", tc.bucket, err, tc.want)
		}
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
