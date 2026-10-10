package sse

import (
	"bytes"
	"reflect"
	"testing"
)

type event struct{ Event, Data string }

func scanAll(b []byte) []event {
	var out []event
	sc := NewScanner(b)
	for sc.Next() {
		out = append(out, event{string(sc.Event()), string(sc.Data())})
	}
	return out
}

func TestScanner(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []event
	}{
		{"single data line", "data: {}\n\n", []event{{"", "{}"}}},
		{"multi-line data in one event", "data: {\"a\":\ndata: 1}\n\n", []event{{"", "{\"a\":\n1}"}}},
		{"three data lines", "data: a\ndata: b\ndata: c\n\n", []event{{"", "a\nb\nc"}}},
		{"several events in one chunk", "data: 1\n\ndata: 2\n\ndata: 3\n\n", []event{{"", "1"}, {"", "2"}, {"", "3"}}},
		{"event and data", "event: ping\ndata: {}\n\nevent: stop\ndata: {\"x\":1}\n\n", []event{{"ping", "{}"}, {"stop", "{\"x\":1}"}}},
		{"event name resets between events", "event: a\ndata: 1\n\ndata: 2\n\n", []event{{"a", "1"}, {"", "2"}}},
		{"comment lines ignored", ": keepalive\ndata: 1\n: mid\ndata: 2\n\n", []event{{"", "1\n2"}}},
		{"comment-only chunk", ": keepalive\n\n", nil},
		{"data without space", "data:x\n\n", []event{{"", "x"}}},
		{"only one leading space stripped", "data:  x\n\n", []event{{"", " x"}}},
		{"empty data line inside event", "data: a\ndata:\ndata: b\n\n", []event{{"", "a\n\nb"}}},
		{"empty data event skipped", "data:\n\ndata: 1\n\n", []event{{"", "1"}}},
		{"event without data skipped", "event: x\n\ndata: 1\n\n", []event{{"", "1"}}},
		{"field with no colon is a data line", "data\ndata: a\n\n", []event{{"", "\na"}}},
		{"unknown fields ignored", "id: 7\nretry: 10\nfoo: bar\ndata: 1\n\n", []event{{"", "1"}}},
		{"no trailing blank line", "event: e\ndata: 1", []event{{"e", "1"}}},
		{"CRLF line endings", "event: e\r\ndata: a\r\ndata: b\r\n\r\ndata: c\r\n\r\n", []event{{"e", "a\nb"}, {"", "c"}}},
		{"CR line endings", "data: a\rdata: b\r\rdata: c\r", []event{{"", "a\nb"}, {"", "c"}}},
		{"empty input", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scanAll([]byte(tc.in)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScannerLeavesInputUnchanged(t *testing.T) {
	in := []byte("data: a\ndata: b\ndata: c\n\ndata: d\n\n")
	orig := bytes.Clone(in)
	scanAll(in)
	if !bytes.Equal(in, orig) {
		t.Errorf("input mutated: %q", in)
	}
}

func TestScannerSingleLineAllocs(t *testing.T) {
	in := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n")
	allocs := testing.AllocsPerRun(100, func() {
		sc := NewScanner(in)
		for sc.Next() {
		}
	})
	if allocs != 0 {
		t.Errorf("allocs = %v, want 0", allocs)
	}
}
