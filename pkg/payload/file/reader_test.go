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

func TestReader_ListStripsBodiesNewestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.jsonl")
	base := time.Now().UTC().Truncate(time.Second)
	writeRecs(t, path,
		payload.Record{RequestID: "old", Timestamp: base, Status: 200, RequestBody: []byte("a")},
		payload.Record{RequestID: "new", Timestamp: base.Add(time.Second), Status: 200, ResponseBody: []byte("b")},
	)

	r := NewReader(path)
	got, err := r.List(context.Background(), payload.Query{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0].RequestID != "new" {
		t.Fatalf("order: want newest first, got %+v", got)
	}
	for _, rec := range got {
		if rec.RequestBody != nil || rec.ResponseBody != nil {
			t.Fatalf("List leaked bodies: %+v", rec)
		}
	}
}

func TestReader_GetReturnsBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.jsonl")
	writeRecs(t, path, payload.Record{
		RequestID:    "req-1",
		Timestamp:    time.Now().UTC(),
		Status:       200,
		RequestBody:  []byte(`{"in":1}`),
		ResponseBody: []byte(`{"out":2}`),
	})

	r := NewReader(path)
	got, err := r.Get(context.Background(), "req-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.RequestBody) != `{"in":1}` || string(got.ResponseBody) != `{"out":2}` {
		t.Fatalf("bodies mismatch: %+v", got)
	}
}

func TestReader_GetNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.jsonl")
	writeRecs(t, path, payload.Record{RequestID: "x", Timestamp: time.Now().UTC(), Status: 200})

	r := NewReader(path)
	_, err := r.Get(context.Background(), "missing")
	if !errors.Is(err, payload.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestReader_MissingFileIsEmpty(t *testing.T) {
	r := NewReader(filepath.Join(t.TempDir(), "absent.jsonl"))
	got, err := r.List(context.Background(), payload.Query{})
	if err != nil {
		t.Fatalf("list on missing file: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %+v", got)
	}
	if _, err := r.Get(context.Background(), "any"); !errors.Is(err, payload.ErrNotFound) {
		t.Fatalf("want ErrNotFound on missing file, got %v", err)
	}
}
