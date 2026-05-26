package file

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/payload"
)

func TestSink_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	s := NewSinkFromWriter(&buf)
	rec := payload.Record{
		RequestID:    "req-1",
		Timestamp:    time.Now().UTC().Truncate(time.Second),
		Source:       "pipeline",
		Status:       200,
		RequestBody:  []byte(`{"in":1}`),
		ResponseBody: []byte(`{"out":2}`),
	}
	if err := s.Write(rec); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got payload.Record
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RequestID != "req-1" || string(got.RequestBody) != `{"in":1}` || string(got.ResponseBody) != `{"out":2}` {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
