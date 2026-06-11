package usagelog

import (
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
)

func TestParseTags_Happy(t *testing.T) {
	tags, ok := ParseTags(`{"session_id":"0988342e","leg":"translate"}`)
	if !ok {
		t.Fatal("want ok")
	}
	if len(tags) != 2 || tags["session_id"] != "0988342e" || tags["leg"] != "translate" {
		t.Fatalf("tags: %+v", tags)
	}
}

func TestParseTags_Violations(t *testing.T) {
	manyKeys := "{"
	for i := 0; i < maxTagCount+1; i++ {
		if i > 0 {
			manyKeys += ","
		}
		manyKeys += `"k` + string(rune('a'+i)) + `":"v"`
	}
	manyKeys += "}"

	cases := map[string]string{
		"invalid json":     `{not json`,
		"top-level array":  `["a","b"]`,
		"top-level string": `"a"`,
		"number value":     `{"a":1}`,
		"bool value":       `{"a":true}`,
		"null value":       `{"a":null}`,
		"nested object":    `{"a":{"b":"c"}}`,
		"array value":      `{"a":["b"]}`,
		"too many keys":    manyKeys,
		"empty key":        `{"":"v"}`,
		"key too long":     `{"` + strings.Repeat("k", 65) + `":"v"}`,
		"value too long":   `{"k":"` + strings.Repeat("v", 257) + `"}`,
		"empty object":     `{}`,
		"empty string":     ``,
		"oversized blob":   `{"k":"` + strings.Repeat("v", MaxTagsHeaderBytes) + `"}`,
	}
	for name, raw := range cases {
		if tags, ok := ParseTags(raw); ok {
			t.Errorf("%s: want drop, got %+v", name, tags)
		}
	}
}

func TestBuildEvent_EventTimeAndTags(t *testing.T) {
	start := time.Now().UTC()
	lc := lifecycle.NewContext("req-1", "pipeline", start)
	lc.Metadata["client_ip"] = "10.0.0.1"
	lc.Metadata[MetadataKeyRequestTags] = `{"session_id":"s1"}`

	ev := buildEvent(lc, 200, "", "", nil)
	if !ev.Timestamp.Equal(start) {
		t.Fatalf("timestamp: want Timing.Start %v, got %v", start, ev.Timestamp)
	}
	if ev.Tags["session_id"] != "s1" {
		t.Fatalf("tags: %+v", ev.Tags)
	}
	if ev.Extras["client_ip"] != "10.0.0.1" {
		t.Fatalf("extras: %+v", ev.Extras)
	}
	if _, ok := ev.Extras[MetadataKeyRequestTags]; ok {
		t.Fatalf("raw tags leaked into extras: %+v", ev.Extras)
	}

	// EventTime set overrides the timestamp; durations stay anchored on Start.
	eventTime := time.Date(2025, 11, 3, 9, 30, 0, 0, time.UTC)
	lc.EventTime = eventTime
	ev = buildEvent(lc, 200, "", "", nil)
	if !ev.Timestamp.Equal(eventTime) {
		t.Fatalf("timestamp: want EventTime %v, got %v", eventTime, ev.Timestamp)
	}

	// Invalid tag blob drops Tags, leaves the event intact.
	lc.Metadata[MetadataKeyRequestTags] = `{"a":1}`
	ev = buildEvent(lc, 200, "", "", nil)
	if ev.Tags != nil {
		t.Fatalf("invalid blob: want nil tags, got %+v", ev.Tags)
	}
	if ev.Status != 200 || ev.RequestID != "req-1" {
		t.Fatalf("event damaged: %+v", ev)
	}
}
