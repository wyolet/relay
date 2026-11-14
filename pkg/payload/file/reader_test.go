package file

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/payload"
)

var base = time.Date(2026, 5, 26, 14, 30, 0, 0, time.UTC)

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

// One record is one JSON line, with bodies base64-encoded and unset fields left out.
func TestSinkWritesOneLinePerRecord(t *testing.T) {
	var buf bytes.Buffer
	s := NewSinkFromWriter(&buf)
	for _, r := range []payload.Record{
		{RequestID: "req-1", Timestamp: base, ProjectID: "p", RequestBody: []byte(`{"in":"<1>"}`), ResponseTruncated: true},
		{RequestID: "req-2", Timestamp: base},
	} {
		if err := s.Write(r); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	want := `{"request_id":"req-1","ts":"2026-05-26T14:30:00Z","project_id":"p","request_body":"eyJpbiI6IjwxPiJ9","response_truncated":true}` + "\n" +
		`{"request_id":"req-2","ts":"2026-05-26T14:30:00Z"}` + "\n"
	if diff := cmp.Diff(want, buf.String()); diff != "" {
		t.Errorf("lines written (-want +got):\n%s", diff)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close of a writer-backed sink: %v", err)
	}
	if _, err := NewSink(filepath.Join(t.TempDir(), "no-such-dir", "p.jsonl")); err == nil || !strings.HasPrefix(err.Error(), "payload/file.NewSink: open ") {
		t.Errorf("NewSink in a missing directory: err = %v", err)
	}
}

func TestReaderGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.jsonl")
	full := payload.Record{
		RequestID: "req-1", Timestamp: base, ProjectID: "proj", PrincipalID: "sa-1", RelayKeyHash: "hash",
		RequestBody: []byte(`{"in":1}`), ResponseBody: []byte("binary \x00\xff"), RequestTruncated: true, ResponseTruncated: true,
	}
	// A request written more than once reads as its latest record, wherever it sits in the file.
	newest := payload.Record{RequestID: "twice", Timestamp: base.Add(time.Minute), ResponseBody: []byte("newest")}
	big := payload.Record{RequestID: "big", Timestamp: base, ResponseBody: bytes.Repeat([]byte("x"), 200<<10)}
	writeRecs(t, path, full,
		payload.Record{RequestID: "twice", Timestamp: base, ResponseBody: []byte("older")},
		newest,
		payload.Record{RequestID: "twice", Timestamp: base.Add(-time.Minute), ResponseBody: []byte("oldest")},
		big,
	)
	// Lines that are not records are skipped, not fatal.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\nnot json\n{\"request_id\":\"torn\"\n"); err != nil {
		t.Fatal(err)
	}
	last := payload.Record{RequestID: "last", Timestamp: base}
	_ = f.Close()
	writeRecs(t, path, last)

	r := NewReader(path)
	for _, want := range []payload.Record{full, newest, big, last} {
		got, err := r.Get(context.Background(), want.RequestID)
		if err != nil {
			t.Errorf("Get(%s): %v", want.RequestID, err)
			continue
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Get(%s) (-written +read):\n%s", want.RequestID, diff)
		}
	}
	for _, id := range []string{"missing", "torn", "req", ""} {
		if got, err := r.Get(context.Background(), id); !errors.Is(err, payload.ErrNotFound) {
			t.Errorf("Get(%q) = %+v, %v; want ErrNotFound", id, got, err)
		}
	}
}

func TestReaderGetFileErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewReader(filepath.Join(dir, "absent.jsonl")).Get(context.Background(), "any"); !errors.Is(err, payload.ErrNotFound) {
		t.Errorf("missing file: err = %v, want ErrNotFound", err)
	}
	if _, err := NewReader(dir).Get(context.Background(), "any"); err == nil || errors.Is(err, payload.ErrNotFound) || !strings.HasPrefix(err.Error(), "payload/file.Reader: ") {
		t.Errorf("unreadable path: err = %v, want a reader error that is not ErrNotFound", err)
	}
	// A line past the 16 MiB scan limit fails the lookup rather than hiding the record.
	huge := filepath.Join(dir, "huge.jsonl")
	writeRecs(t, huge, payload.Record{RequestID: "huge", Timestamp: base, ResponseBody: make([]byte, 13<<20)})
	if _, err := NewReader(huge).Get(context.Background(), "huge"); err == nil || !strings.HasPrefix(err.Error(), "payload/file.Reader: scan: ") {
		t.Errorf("oversized line: err = %v, want a scan error", err)
	}
}
