package inference

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/lifecycle"
)

func TestApplyObsHeaders(t *testing.T) {
	mk := func() *lifecycle.Context {
		return lifecycle.NewContext("req", "pipeline", time.Now())
	}
	h := http.Header{}
	h.Set(httpheader.HeaderEventTime, "2025-11-03T09:30:00.5Z")
	h.Set(httpheader.HeaderRequestTags, `{"session_id":"s1"}`)

	// Flag off: event time ignored, tags still captured raw (unparsed).
	lc := mk()
	applyObsHeaders(lc, h, false)
	if !lc.EventTime.IsZero() {
		t.Fatalf("flag off: want zero EventTime, got %v", lc.EventTime)
	}
	if lc.Metadata[usagelog.MetadataKeyRequestTags] != `{"session_id":"s1"}` {
		t.Fatalf("tags metadata: %+v", lc.Metadata)
	}

	// Flag on: RFC3339Nano parsed; Timing.Start untouched.
	lc = mk()
	start := lc.Timing.Start
	applyObsHeaders(lc, h, true)
	want := time.Date(2025, 11, 3, 9, 30, 0, 500_000_000, time.UTC)
	if !lc.EventTime.Equal(want) {
		t.Fatalf("EventTime: want %v, got %v", want, lc.EventTime)
	}
	if !lc.Timing.Start.Equal(start) {
		t.Fatal("Timing.Start must never change")
	}

	// Unparsable value is silently ignored even with the flag on.
	bad := http.Header{}
	bad.Set(httpheader.HeaderEventTime, "yesterday at noon")
	lc = mk()
	applyObsHeaders(lc, bad, true)
	if !lc.EventTime.IsZero() {
		t.Fatalf("bad value: want zero EventTime, got %v", lc.EventTime)
	}

	// Oversized tags header is never copied.
	big := http.Header{}
	big.Set(httpheader.HeaderRequestTags, `{"k":"`+strings.Repeat("v", usagelog.MaxTagsHeaderBytes)+`"}`)
	lc = mk()
	applyObsHeaders(lc, big, false)
	if _, ok := lc.Metadata[usagelog.MetadataKeyRequestTags]; ok {
		t.Fatal("oversized tags header must not be copied")
	}
}

// Both observability headers must sit inside the X-WR-* strip denylist so
// they never reach the upstream.
func TestObsHeadersAreStripped(t *testing.T) {
	for _, name := range []string{httpheader.HeaderEventTime, httpheader.HeaderRequestTags} {
		if !httpheader.Match(name, httpheader.StripDenylist) {
			t.Fatalf("%s is not covered by the strip denylist", name)
		}
	}
}
