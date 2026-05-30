package file

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/payload"
)

func writeRecs(t *testing.T, path string, recs ...payload.Record) {
	t.Helper()
	s, err := NewSink(path)
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	for _, r := range recs {
		if err := s.Write(r); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestReader_GetReturnsBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.jsonl")
	writeRecs(t, path, payload.Record{
		RequestID:    "req-1",
		Timestamp:    time.Now().UTC(),
		RequestBody:  []byte(`{"in":1}`),
		ResponseBody: []byte(`{"out":2}`),
	})

	got, err := NewReader(path).Get(context.Background(), "req-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.RequestBody) != `{"in":1}` || string(got.ResponseBody) != `{"out":2}` {
		t.Fatalf("bodies mismatch: %+v", got)
	}
}

func TestReader_GetNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.jsonl")
	writeRecs(t, path, payload.Record{RequestID: "x", Timestamp: time.Now().UTC()})

	if _, err := NewReader(path).Get(context.Background(), "missing"); !errors.Is(err, payload.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestReader_MissingFileIsNotFound(t *testing.T) {
	r := NewReader(filepath.Join(t.TempDir(), "absent.jsonl"))
	if _, err := r.Get(context.Background(), "any"); !errors.Is(err, payload.ErrNotFound) {
		t.Fatalf("want ErrNotFound on missing file, got %v", err)
	}
}
