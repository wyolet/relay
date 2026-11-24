package pipeline

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/plan"
)

// A spent budget records its own usage kind; a rate rule and an upstream limit record rate_limited.
func TestClassifyFailure_LimitRefusals(t *testing.T) {
	refusedBy := func(k limits.Kind) error {
		return &CallerLimitedError{Refused: &limits.Refused{Counter: &limits.Counter{Kind: k}}}
	}
	for want, err := range map[string]error{
		"budget_exceeded": refusedBy(limits.KindBudget),
		"rate_limited":    refusedBy(limits.KindRule),
	} {
		if kind, status := classifyFailure(err); kind != want || status != 0 {
			t.Errorf("classifyFailure(%v) = %q, %d; want %q, 0", err, kind, status, want)
		}
	}
	// A provider account's spent budget is answered as an upstream rate limit, so it records as one.
	for _, err := range []error{&UpstreamLimitedError{}, &UpstreamLimitedError{Refused: &limits.Refused{Counter: &limits.Counter{Kind: limits.KindBudget, Rung: limits.RungAccount}}}} {
		if kind, _ := classifyFailure(err); kind != "rate_limited" {
			t.Errorf("an upstream limit (%+v) records %q, want rate_limited", err, kind)
		}
	}
}

func TestErrorStrings(t *testing.T) {
	long := strings.Repeat("x", errorExcerpt+100)
	for want, err := range map[string]error{
		"upstream returned 503: (empty body)":                                 &UpstreamFailureError{Status: 503},
		"upstream returned 400: {\"error\":\"bad\"}":                          &UpstreamFailureError{Status: 400, Body: []byte(" {\"error\":\"bad\"}\n")},
		"upstream returned 500: " + long[:errorExcerpt]:                       &UpstreamFailureError{Status: 500, Body: []byte(long)},
		`upstream host "h" unreachable after 3 attempt(s): boom`:              &UpstreamUnreachableError{Host: "h", Attempts: 3, Cause: errors.New("boom")},
		"limits: project policy pol refused counter c-1":                      &CallerLimitedError{Refused: &limits.Refused{Counter: &limits.Counter{Rung: limits.RungProject, PolicyID: "pol", ID: "c-1"}}},
		"pipeline: every credential for this model is over an upstream limit": &UpstreamLimitedError{},
	} {
		if got := err.Error(); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	}
	cause := errors.New("dial")
	if !errors.Is(&UpstreamFailureError{Cause: cause}, cause) || !errors.Is(&UpstreamUnreachableError{Cause: cause}, cause) {
		t.Error("upstream errors do not unwrap to their cause")
	}
	if got := (&Request{ModelName: "m", Hops: make([]plan.RouteHop, 2)}).String(); got != `pipeline.Request{model="m" hops=2}` {
		t.Errorf("Request.String() = %q", got)
	}
}

// closeCounter counts the closes of a response body.
type closeCounter struct {
	io.Reader
	closed int
}

func (c *closeCounter) Close() error { c.closed++; return nil }

// The retained error body is the decoded payload, cut at the cap, and the response body is always drained and closed.
func TestReadBodyCapped(t *testing.T) {
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write([]byte("decoded error"))
	_ = zw.Close()
	for _, tc := range []struct {
		name     string
		body     string
		encoding string
		max      int
		want     string
	}{
		{name: "short body", body: "rejected", max: 64, want: "rejected"},
		{name: "cut at the cap", body: "0123456789", max: 4, want: "0123"},
		{name: "empty body", max: 64},
		{name: "gzip is decoded", body: zipped.String(), encoding: "GZIP", max: 64, want: "decoded error"},
	} {
		src := &closeCounter{Reader: strings.NewReader(tc.body)}
		resp := &http.Response{Body: src, Header: http.Header{}}
		if tc.encoding != "" {
			resp.Header.Set("Content-Encoding", tc.encoding)
		}
		got := readBodyCapped(resp, tc.max)
		if string(got) != tc.want || (tc.want == "") != (got == nil) {
			t.Errorf("%s: read %q, want %q", tc.name, got, tc.want)
		}
		if rest, _ := io.ReadAll(src.Reader); src.closed != 1 || len(rest) != 0 {
			t.Errorf("%s: body closed %d times with %d bytes unread, want closed once and drained", tc.name, src.closed, len(rest))
		}
	}
	if readBodyCapped(nil, 8) != nil || readBodyCapped(&http.Response{}, 8) != nil {
		t.Error("a missing response or body yielded bytes")
	}
}

func TestReadBodyCapped_BodyMislabelledAsGzipIsKept(t *testing.T) {
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("plain")), Header: http.Header{"Content-Encoding": {"gzip"}}}
	if got := readBodyCapped(resp, 64); string(got) != "plain" {
		t.Errorf("read %q, want the body as sent", got)
	}
}

func TestConnBackoff(t *testing.T) {
	for attempt, want := range map[int]time.Duration{1: 100 * time.Millisecond, 2: 200 * time.Millisecond, 5: 1600 * time.Millisecond, 6: 2 * time.Second, 20: 2 * time.Second} {
		if got := connBackoff(attempt); got != want {
			t.Errorf("connBackoff(%d) = %v, want %v", attempt, got, want)
		}
	}
}

func TestRetryAfterHeader(t *testing.T) {
	in := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(http.TimeFormat) }
	for _, tc := range []struct {
		name, value string
		min, max    time.Duration
	}{
		{name: "absent"},
		{name: "seconds", value: "7", min: 7 * time.Second, max: 7 * time.Second},
		{name: "zero seconds", value: "0"},
		{name: "a date ahead", value: in(time.Minute), min: 58 * time.Second, max: time.Minute},
		{name: "a date already past", value: in(-time.Minute)},
		{name: "neither", value: "soon"},
	} {
		h := http.Header{}
		if tc.value != "" {
			h.Set("Retry-After", tc.value)
		}
		if got := RetryAfterHeader(h); got < tc.min || got > tc.max {
			t.Errorf("%s: RetryAfterHeader(%q) = %v, want within [%v, %v]", tc.name, tc.value, got, tc.min, tc.max)
		}
	}
}

// A hop keeps the caller hold only when its caller counters are the ones held: same buckets, in the same atomicity groups.
func TestSameCounters(t *testing.T) {
	a := []limits.Counter{{ID: "c1", Group: "g"}, {ID: "c2", Group: "g"}}
	for _, tc := range []struct {
		name string
		b    []limits.Counter
		want bool
	}{
		{"the same", []limits.Counter{{ID: "c1", Group: "g"}, {ID: "c2", Group: "g"}}, true},
		{"one fewer", a[:1], false},
		{"another bucket", []limits.Counter{{ID: "c1", Group: "g"}, {ID: "c3", Group: "g"}}, false},
		{"another group", []limits.Counter{{ID: "c1", Group: "g"}, {ID: "c2", Group: "h"}}, false},
		{"another order", []limits.Counter{{ID: "c2", Group: "g"}, {ID: "c1", Group: "g"}}, false},
	} {
		if got := sameCounters(a, tc.b); got != tc.want {
			t.Errorf("%s: sameCounters = %v, want %v", tc.name, got, tc.want)
		}
	}
	if !sameCounters(nil, nil) {
		t.Error("two empty lists differ")
	}
}
