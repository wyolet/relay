//go:build integration

// Live ClickHouse round trip of the message content recorded from client telemetry. Runs only with -tags=integration AND RELAY_TEST_CH_DSN set (else skipped).
//
//	RELAY_TEST_CH_DSN=clickhouse://default@host:9000/relay \
//	  go test -tags=integration ./pkg/payload/clickhouse/ -run Integration -v

package clickhouse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/payload"
)

func TestIntegration_ReportedContentRoundTrip(t *testing.T) {
	dsn := os.Getenv("RELAY_TEST_CH_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_CH_DSN unset; skipping live ClickHouse smoke")
	}

	s, err := New(Config{
		DSN:           dsn,
		WALDir:        t.TempDir(),
		FlushInterval: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	ids := make([]byte, 24)
	if _, err := rand.Read(ids); err != nil {
		t.Fatalf("rand: %v", err)
	}
	// Request ids as the receiver derives them from a span's ids; the trace id is this run's marker.
	trace := "otlp-" + hex.EncodeToString(ids[:16])
	t.Cleanup(func() { deleteMarkerRows(t, dsn, trace) })
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Bodies in the stored form: one JSON object per side keyed by attribute name, HTML characters unescaped.
	complete := payload.Record{
		RequestID:    trace + "-" + hex.EncodeToString(ids[16:]),
		Timestamp:    now,
		RequestBody:  []byte(`{"gen_ai.input.messages":[{"parts":[{"content":"Say hello. <b>bold</b> & 1.0 — ok","type":"text"}],"role":"user"}],"gen_ai.system_instructions":[{"content":"You are a terse assistant.","type":"text"}]}`),
		ResponseBody: []byte(`{"gen_ai.output.messages":[{"finish_reason":"stop","parts":[{"content":"Hello.","type":"text"}],"role":"assistant"}]}`),
		ProjectID:    "p-1",
		PrincipalID:  "sa-1",
		RelayKeyHash: "h-1",
	}
	// A failed call reports its input only; a body over the cap is cut mid-document and flagged.
	inputOnly := payload.Record{
		RequestID:        trace + "-" + "00000000000000ff",
		Timestamp:        now.Add(time.Second),
		RequestBody:      []byte(`{"gen_ai.input.messages":[{"parts":[{"content":"Say he`),
		RequestTruncated: true,
		ProjectID:        "p-1",
		PrincipalID:      "sa-1",
		RelayKeyHash:     "h-1",
	}
	for _, rec := range []payload.Record{complete, inputOnly} {
		if err := s.Write(rec); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	ctx := context.Background()
	fetch := func(requestID string) payload.Record {
		t.Helper()
		var got payload.Record
		var err error
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			got, err = s.Get(ctx, requestID)
			if err == nil {
				return got
			}
			if !errors.Is(err, payload.ErrNotFound) {
				t.Fatalf("Get: %v", err)
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("Get never landed: %v", err)
		return got
	}

	got := fetch(complete.RequestID)
	if string(got.RequestBody) != string(complete.RequestBody) || string(got.ResponseBody) != string(complete.ResponseBody) {
		t.Errorf("body round-trip mismatch: req=%q resp=%q", got.RequestBody, got.ResponseBody)
	}
	if got.RequestTruncated || got.ResponseTruncated || !got.Timestamp.Equal(now) {
		t.Errorf("flags or time mismatch: %+v", got)
	}
	if got.ProjectID != "p-1" || got.PrincipalID != "sa-1" || got.RelayKeyHash != "h-1" {
		t.Errorf("owner round-trip mismatch: %+v", got)
	}

	got = fetch(inputOnly.RequestID)
	if string(got.RequestBody) != string(inputOnly.RequestBody) || got.ResponseBody != nil {
		t.Errorf("input-only round-trip mismatch: req=%q resp=%q", got.RequestBody, got.ResponseBody)
	}
	if !got.RequestTruncated || got.ResponseTruncated {
		t.Errorf("truncation flags mismatch: %+v", got)
	}
}
