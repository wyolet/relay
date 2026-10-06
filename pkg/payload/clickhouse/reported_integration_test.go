//go:build integration

// Live ClickHouse round trip of the message content recorded from client telemetry. Runs only with -tags=integration AND RELAY_TEST_CH_DSN set (else skipped).
//
//	RELAY_TEST_CH_DSN=clickhouse://default@host:9000/relay \
//	  go test -tags=integration ./pkg/payload/clickhouse/ -run Integration -v

package clickhouse

import (
	"bytes"
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

// reportedTurn reads one of the request bodies the telemetry receiver stores for two consecutive turns of a conversation. The receiver's own tests hold the files to what it builds.
func reportedTurn(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(b, []byte("\n"))
}

// Reported content names its arrays by telemetry attribute, not by a provider's request fields; the split knows no wire shape, so the second turn stores only its two new messages.
func TestIntegration_ReportedContentIsDeduplicated(t *testing.T) {
	dsn, conn := throwawayDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	turns := []payload.Record{
		{
			RequestID: "otlp-5b8efff798038103d269b633813fc60c-eee19b7ec3c1b174", Timestamp: now,
			RequestBody:  reportedTurn(t, "reported_turn1_request.json"),
			ResponseBody: []byte(`{"gen_ai.output.messages":[{"finish_reason":"tool_calls","parts":[{"arguments":{"city":"Tashkent","days":3,"min_confidence":1},"id":"call_1","name":"get_weather","type":"tool_call"}],"role":"assistant"}]}`),
			ProjectID:    "p-1", PrincipalID: "sa-1", RelayKeyHash: "h-1",
		},
		{
			RequestID: "otlp-5b8efff798038103d269b633813fc60c-eee19b7ec3c1b175", Timestamp: now.Add(time.Second),
			RequestBody:  reportedTurn(t, "reported_turn2_request.json"),
			ResponseBody: []byte(`{"gen_ai.output.messages":[{"finish_reason":"stop","parts":[{"content":"Friday in Tashkent will be clear, 31.5 degrees at most (met office).","type":"text"}],"role":"assistant"}]}`),
			ProjectID:    "p-1", PrincipalID: "sa-1", RelayKeyHash: "h-1",
		},
	}

	s, err := New(Config{DSN: dsn, WALDir: t.TempDir(), FlushInterval: 200 * time.Millisecond, Dedup: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// One flush per turn, as two exports minutes apart would arrive.
	writeAll(t, s, turns[:1])
	waitForRecord(t, s, turns[0].RequestID)
	afterFirst := countRows(t, conn, "SELECT count() FROM payload_pieces")
	writeAll(t, s, turns[1:])
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// One message, one instruction part and two tool definitions; then the two messages the second turn adds.
	total := countRows(t, conn, "SELECT count() FROM payload_pieces")
	if afterFirst != 4 || total != 6 {
		t.Errorf("payload_pieces rows = %d after the first turn and %d after the second, want 4 and 6", afterFirst, total)
	}
	if n := countRows(t, conn, "SELECT count() FROM payload_requests"); n != 2 {
		t.Errorf("payload_requests rows = %d, want 2", n)
	}
	if n := countRows(t, conn, "SELECT count() FROM payload_requests WHERE has(field_names, 'gen_ai.input.messages') AND has(field_names, 'gen_ai.tool.definitions') AND has(field_names, 'gen_ai.system_instructions') AND arrayAll(x -> x = 1, field_is_array)"); n != 2 {
		t.Errorf("%d of 2 request rows split all three content arrays per element", n)
	}

	rdr, err := NewReader(Config{DSN: dsn})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer rdr.Close()
	for _, want := range turns {
		got, err := rdr.Get(ctx, want.RequestID)
		if err != nil {
			t.Fatalf("Get %s: %v", want.RequestID, err)
		}
		assertSameRecord(t, got, want)
	}

	whole := uint64(len(turns[0].RequestBody) + len(turns[1].RequestBody))
	stored := countRows(t, conn, "SELECT sum(length(body)) FROM payload_pieces") + countRows(t, conn, "SELECT sum(length(skeleton)) FROM payload_requests")
	if stored >= whole {
		t.Errorf("deduplicated request bodies take %d bytes, the whole bodies %d", stored, whole)
	}
	t.Logf("two turns: request bodies %d bytes whole, %d bytes as skeletons and %d pieces (%d piece references)",
		whole, stored, total, countRows(t, conn, "SELECT sum(length(hashes)) FROM payload_requests"))
}
