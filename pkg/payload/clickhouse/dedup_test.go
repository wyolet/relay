package clickhouse

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/payload"
	"github.com/wyolet/relay/pkg/payload/dedup"
)

func TestRecentPieces(t *testing.T) {
	r := newRecentPieces(3)
	a := dedup.SplitBody([]byte("a")).Pieces[0]
	b := dedup.SplitBody([]byte("b")).Pieces[0]
	c := dedup.SplitBody([]byte("c")).Pieces[0]

	r.mark([]dedup.Piece{a, b}, 10)
	if !r.writtenOn(a.Hash, 10) || !r.writtenOn(b.Hash, 10) {
		t.Fatal("marked pieces not remembered")
	}
	if r.writtenOn(a.Hash, 11) {
		t.Fatal("a piece written yesterday must be written again today")
	}
	r.mark([]dedup.Piece{c, a}, 11) // 2 + 2 > 3 clears first
	if r.writtenOn(b.Hash, 10) || len(r.day) != 2 {
		t.Fatalf("overflow did not clear: %d entries", len(r.day))
	}
	if !r.writtenOn(a.Hash, 11) || !r.writtenOn(c.Hash, 11) {
		t.Fatal("pieces marked after clear not remembered")
	}
}

func TestUTCDay(t *testing.T) {
	late := time.Date(2026, 10, 6, 23, 59, 59, 0, time.UTC)
	if utcDay(late) != utcDay(late.Add(-23*time.Hour)) || utcDay(late) == utcDay(late.Add(time.Second)) {
		t.Fatal("utcDay does not change at UTC midnight")
	}
	plusFive := time.FixedZone("UTC+5", 5*3600)
	if utcDay(late.In(plusFive)) != utcDay(late) {
		t.Fatal("utcDay depends on the time's zone")
	}
}

// The request row's field columns must rebuild the exact body.
func TestRequestValuesRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","system":[{"type":"text","text":"s"}],"messages":[{"a":1},{"a":1},{"b":2}],"tools":[],"stream":true}`)
	split := dedup.SplitBody(body)
	v := requestValues(payload.Record{RequestID: "r", RequestBody: body}, split)

	names := v[6].([]string)
	isArray := v[7].([]uint8)
	offsets := v[8].([]uint32)
	counts := v[9].([]uint32)
	var hashes []string
	for _, h := range v[10].([][]byte) {
		hashes = append(hashes, string(h))
	}
	fields, err := fieldsFromColumns(names, isArray, offsets, counts, hashes)
	if err != nil {
		t.Fatalf("fieldsFromColumns: %v", err)
	}
	pieces := map[dedup.Hash][]byte{}
	for _, p := range split.Pieces {
		pieces[p.Hash] = p.Body
	}
	got, err := dedup.Rebuild([]byte(v[5].(string)), fields, func(h dedup.Hash) ([]byte, bool) {
		b, ok := pieces[h]
		return b, ok
	})
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("rebuild = %q, %v; want %q", got, err, body)
	}
}

func TestFieldsFromColumnsRejectsCorruptRows(t *testing.T) {
	h := string(make([]byte, 16))
	for name, c := range map[string]struct {
		names   []string
		isArray []uint8
		offsets []uint32
		counts  []uint32
		hashes  []string
	}{
		"column lengths differ": {[]string{"a"}, nil, []uint32{0}, []uint32{1}, []string{h}},
		"counts exceed hashes":  {[]string{"a"}, []uint8{1}, []uint32{0}, []uint32{2}, []string{h}},
		"unclaimed hashes":      {[]string{"a"}, []uint8{1}, []uint32{0}, []uint32{1}, []string{h, h}},
		"short hash":            {[]string{"a"}, []uint8{1}, []uint32{0}, []uint32{1}, []string{"x"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := fieldsFromColumns(c.names, c.isArray, c.offsets, c.counts, c.hashes)
			if !errors.Is(err, errCorruptRow) {
				t.Fatalf("err = %v, want errCorruptRow", err)
			}
		})
	}
}

func TestPiecesRetentionDays(t *testing.T) {
	for in, want := range map[int]int{0: 0, 1: 2, 30: 31} {
		if got := piecesRetentionDays(in); got != want {
			t.Fatalf("piecesRetentionDays(%d) = %d, want %d", in, got, want)
		}
	}
}
