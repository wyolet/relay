//go:build integration

package clickhouse

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/payload"
	"github.com/wyolet/relay/pkg/payload/dedup"
)

func chatRecord(id, project, principal string, ts time.Time, messages ...string) payload.Record {
	body := `{"model":"m","messages":[`
	for i, m := range messages {
		if i > 0 {
			body += ","
		}
		body += m
	}
	return payload.Record{
		RequestID: id, Timestamp: ts, ProjectID: project, PrincipalID: principal,
		RequestBody: []byte(body + `]}`), ResponseBody: []byte(`{"reply":"` + id + `"}`),
	}
}

func assertReadsBack(t *testing.T, r payload.Reader, recs []payload.Record) {
	t.Helper()
	for _, want := range recs {
		got, err := r.Get(context.Background(), want.RequestID)
		if err != nil {
			t.Fatalf("Get %s: %v", want.RequestID, err)
		}
		assertSameRecord(t, got, want)
	}
}

func assertErased(t *testing.T, r payload.Reader, recs []payload.Record) {
	t.Helper()
	for _, rec := range recs {
		if _, err := r.Get(context.Background(), rec.RequestID); !errors.Is(err, payload.ErrNotFound) {
			t.Fatalf("Get erased %s: err = %v, want ErrNotFound", rec.RequestID, err)
		}
	}
}

func TestIntegration_EraseByProject(t *testing.T) {
	dsn, conn := throwawayDatabase(t)
	start := time.Now().UTC().Truncate(time.Microsecond)
	var erased, kept []payload.Record
	for _, rec := range conversationRecords(start) {
		erased = append(erased, rec)
		rec.RequestID, rec.ProjectID = "p2-"+rec.RequestID, "p-2"
		kept = append(kept, rec)
	}
	writeDedup(t, dsn, append(append([]payload.Record{}, erased...), kept...))

	// Whole-body rows of both projects, from before dedup was switched on.
	wholeErased := chatRecord("whole-1", "p-1", "u-1", start, `{"role":"user","content":"old"}`)
	wholeKept := chatRecord("whole-2", "p-2", "u-1", start, `{"role":"user","content":"old"}`)
	s, err := New(Config{DSN: dsn, WALDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writeAll(t, s, []payload.Record{wholeErased, wholeKept})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	keptPieces := countRows(t, conn, "SELECT count() FROM payload_pieces FINAL WHERE project_id = 'p-2'")
	rdr := openReader(t, dsn)
	res, err := rdr.Erase(context.Background(), payload.EraseFilter{ProjectID: "p-1"})
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if want := uint64(len(erased) + 1); res.Requests != want {
		t.Fatalf("Erase matched %d requests, want %d", res.Requests, want)
	}

	for _, table := range []string{"payload_requests", "payload_pieces", "payload_logs"} {
		if n := countRows(t, conn, "SELECT count() FROM "+table+" WHERE project_id = 'p-1'"); n != 0 {
			t.Fatalf("%s keeps %d rows of the erased project", table, n)
		}
	}
	assertErased(t, rdr, append(erased, wholeErased))
	if n := countRows(t, conn, "SELECT count() FROM payload_pieces FINAL WHERE project_id = 'p-2'"); n != keptPieces {
		t.Fatalf("other project's pieces = %d, want %d", n, keptPieces)
	}
	assertReadsBack(t, rdr, append(kept, wholeKept))
}

func TestIntegration_EraseByPrincipal(t *testing.T) {
	dsn, conn := throwawayDatabase(t)
	const (
		shared   = `{"role":"system","content":"shared by both principals"}`
		ownedU1  = `{"role":"user","content":"only u-1 sent this"}`
		ownedU2  = `{"role":"user","content":"only u-2 sent this"}`
		otherPrj = `{"role":"user","content":"u-1 in another project"}`
	)
	now := time.Now().UTC().Truncate(time.Microsecond)
	u1 := []payload.Record{
		chatRecord("u1-a", "p-1", "u-1", now, shared, ownedU1),
		chatRecord("u1-b", "p-2", "u-1", now, otherPrj),
	}
	u2 := []payload.Record{chatRecord("u2-a", "p-1", "u-2", now, shared, ownedU2)}
	writeDedup(t, dsn, append(append([]payload.Record{}, u1...), u2...))

	rdr := openReader(t, dsn)
	res, err := rdr.Erase(context.Background(), payload.EraseFilter{PrincipalID: "u-1"})
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if res.Requests != uint64(len(u1)) {
		t.Fatalf("Erase matched %d requests, want %d", res.Requests, len(u1))
	}

	if n := countRows(t, conn, "SELECT count() FROM payload_requests WHERE principal_id = 'u-1'"); n != 0 {
		t.Fatalf("payload_requests keeps %d rows of the erased principal", n)
	}
	for _, c := range []struct {
		project, element string
		want             uint64
	}{
		{"p-1", shared, 1},
		{"p-1", ownedU2, 1},
		{"p-1", ownedU1, 0},
		{"p-2", otherPrj, 0},
	} {
		if n := countPiece(t, conn, c.project, c.element); n != c.want {
			t.Fatalf("piece %s in %s: %d rows, want %d", c.element, c.project, n, c.want)
		}
	}
	assertErased(t, rdr, u1)
	assertReadsBack(t, rdr, u2)
}

// A sink that wrote a piece today skips it on its next sighting. An erase
// run elsewhere (another pod; here a separate reader) deletes it, and every
// sink must write it again on its next flush.
func TestIntegration_SinksRewritePiecesAfterEraseElsewhere(t *testing.T) {
	dsn, conn := throwawayDatabase(t)
	const msg = `{"role":"user","content":"asked before and after the erase"}`
	now := time.Now().UTC().Truncate(time.Microsecond)
	sinks := make([]*Sink, 2)
	for i := range sinks {
		s, err := New(Config{DSN: dsn, WALDir: t.TempDir(), FlushInterval: 100 * time.Millisecond, Dedup: true})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer s.Close()
		sinks[i] = s
	}

	for i, s := range sinks {
		rec := chatRecord(fmt.Sprintf("before-%d", i), "p-1", "u-1", now, msg)
		writeAll(t, s, []payload.Record{rec})
		waitForRecord(t, s, rec.RequestID)
	}
	if _, err := openReader(t, dsn).Erase(context.Background(), payload.EraseFilter{ProjectID: "p-1"}); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if n := countPiece(t, conn, "p-1", msg); n != 0 {
		t.Fatalf("erase left %d rows of the piece", n)
	}

	for i, s := range sinks {
		after := chatRecord(fmt.Sprintf("after-%d", i), "p-1", "u-1", now.Add(time.Second), msg, `{"role":"user","content":"next"}`)
		writeAll(t, s, []payload.Record{after})
		waitForRecord(t, s, after.RequestID)
		assertReadsBack(t, s, []payload.Record{after})
	}
}

// A sink writes pieces before their request row. Pieces written after an
// erase began must survive its cleanup even though nothing references them
// yet; an unreferenced piece last written before the erase must not.
func TestIntegration_PiecesWrittenDuringEraseSurvive(t *testing.T) {
	dsn, conn := throwawayDatabase(t)
	ctx := context.Background()
	rdr := openReader(t, dsn)
	const stale = `{"role":"user","content":"orphaned before the erase"}`

	old, err := conn.PrepareBatch(ctx, "INSERT INTO payload_pieces (project_id, hash, body, last_seen)")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := hex.DecodeString(pieceHex(stale))
	if err := old.Append("p-1", h, stale, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := old.Send(); err != nil {
		t.Fatal(err)
	}

	var start int64
	if err := conn.QueryRow(ctx, "SELECT toUnixTimestamp64Milli(now64(3))").Scan(&start); err != nil {
		t.Fatal(err)
	}
	rec := chatRecord("in-flight", "p-1", "u-2", time.Now().UTC().Truncate(time.Microsecond),
		`{"role":"user","content":"sent while the erase runs"}`)
	split := dedup.SplitBody(rec.RequestBody)
	pieces, err := conn.PrepareBatch(ctx, insertPiecesSQL)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range split.Pieces {
		if err := pieces.Append("p-1", p.Hash[:], string(p.Body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := pieces.Send(); err != nil {
		t.Fatal(err)
	}

	if err := deleteUnreferencedPieces(syncMutations(ctx), conn, []string{"p-1"}, start); err != nil {
		t.Fatalf("deleteUnreferencedPieces: %v", err)
	}
	if n := countPiece(t, conn, "p-1", stale); n != 0 {
		t.Fatalf("stale unreferenced piece kept (%d rows)", n)
	}

	requests, err := conn.PrepareBatch(ctx, insertRequestsSQL)
	if err != nil {
		t.Fatal(err)
	}
	if err := requests.Append(requestValues(rec, split)...); err != nil {
		t.Fatal(err)
	}
	if err := requests.Send(); err != nil {
		t.Fatal(err)
	}
	assertReadsBack(t, rdr, []payload.Record{rec})
}
