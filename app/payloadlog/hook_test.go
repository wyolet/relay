package payloadlog

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/settings/settingstest"
	"github.com/wyolet/relay/pkg/lifecycle"
)

var recordStart = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// enabledCtrl is a controller with payload logging on, capped at maxBytes, writing to the returned sink.
func enabledCtrl(maxBytes int) (*Controller, *batchSink) {
	sink := openSink()
	c := disabledCtrl()
	c.set(true, maxBytes, settings.PayloadLogging{Enabled: true})
	c.rsink.swap(sink, c.log)
	return c, sink
}

func disabledCtrl() *Controller {
	return NewController(&settingstest.Source{}, func(context.Context, settings.PayloadLogging) (Sink, error) { return openSink(), nil }, EmitterOptions{}, testLogger())
}

// optedIn is a request whose key or policy asked for payload logging.
func optedIn(reqBody string) *lifecycle.Context {
	lc := lifecycle.NewContext("req-1", "pipeline", recordStart)
	lc.PayloadLog = true
	lc.ProjectID, lc.PrincipalID, lc.RelayKeyHash = "p-1", "u-1", "h-1"
	lc.RequestBody = []byte(reqBody)
	return lc
}

// fullRecord is the Record of an optedIn request; an empty resp is no response body at all.
func fullRecord(req, resp string) *Record {
	r := &Record{RequestID: "req-1", Timestamp: recordStart, ProjectID: "p-1", PrincipalID: "u-1", RelayKeyHash: "h-1", RequestBody: []byte(req)}
	if resp != "" {
		r.ResponseBody = []byte(resp)
	}
	return r
}

func TestPayloadHook_Fill(t *testing.T) {
	truncated := func(r *Record, req, resp bool) *Record {
		r.RequestTruncated, r.ResponseTruncated = req, resp
		return r
	}
	optedOut := optedIn("in")
	optedOut.PayloadLog = false
	prefixOnly := optedIn("prefix-only")
	prefixOnly.RequestBodyTruncated = true

	for _, tc := range []struct {
		name     string
		maxBytes int // -1: logging globally off
		lc       *lifecycle.Context
		resp     string
		want     *Record
	}{
		{"captures both bodies and the owner", 0, optedIn(`{"in":1}`), `{"out":2}`, fullRecord(`{"in":1}`, `{"out":2}`)},
		{"request never opted in", 0, optedOut, "out", nil},
		{"logging globally off", -1, optedIn("in"), "out", nil},
		{"no context", 0, nil, "out", nil},
		{"bodies at the cap are whole", 4, optedIn("0123"), "abcd", fullRecord("0123", "abcd")},
		{"bodies past the cap are clipped", 4, optedIn("01234"), "abcde", truncated(fullRecord("0123", "abcd"), true, true)},
		{"only the response is past the cap", 4, optedIn("01"), "abcdefgh", truncated(fullRecord("01", "abcd"), false, true)},
		{"request retained as a prefix", 0, prefixOnly, "resp", truncated(fullRecord("prefix-only", "resp"), true, false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := disabledCtrl()
			if tc.maxBytes >= 0 {
				c, _ = enabledCtrl(tc.maxBytes)
			}
			hook := NewPayloadHook(c)
			v, err := hook.Fill(tc.lc, &lifecycle.PostFlightEvent{Status: 200, ResponseBody: []byte(tc.resp)})
			if err != nil || hook.Name() != Namespace {
				t.Fatalf("Fill: %v (hook %q)", err, hook.Name())
			}
			if tc.want == nil {
				if v != nil {
					t.Fatalf("Fill = %+v, want nothing captured", v)
				}
				return
			}
			if diff := cmp.Diff(tc.want, v.(*Record)); diff != "" {
				t.Fatalf("record (-want +got):\n%s", diff)
			}
		})
	}

	// A request with no start time is stamped now.
	c, _ := enabledCtrl(0)
	lc := lifecycle.NewContext("req", "pipeline", time.Time{})
	lc.PayloadLog = true
	before := time.Now()
	v, _ := NewPayloadHook(c).Fill(lc, &lifecycle.PostFlightEvent{})
	if ts := v.(*Record).Timestamp; ts.Before(before) || ts.After(time.Now()) {
		t.Fatalf("timestamp = %v, want now", ts)
	}
}
