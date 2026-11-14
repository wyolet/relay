package file

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/usage"
)

// Query semantics are pinned by the conformance suite; the tests here cover what is the file reader's own: scanning the file.

func writeFixture(t *testing.T, events []usage.Event) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	sink, err := NewSink(path)
	if err != nil {
		t.Fatalf("file sink: %v", err)
	}
	for _, ev := range events {
		if err := sink.Write(ev); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return path
}

func writeLines(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func requestIDs(t *testing.T, r *Reader) []string {
	t.Helper()
	evs, err := r.Events(context.Background(), usage.EventQuery{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.RequestID)
	}
	return out
}

func TestReaderSkipsBlankAndMalformedLines(t *testing.T) {
	// The long line outgrows the scanner's first 64 KiB buffer; the last line has no trailing newline.
	path := writeLines(t,
		`{"request_id":"first","ts":"2026-01-01T00:00:01Z"}`,
		``,
		`this is not json`,
		`{"request_id":"truncated","ts":"2026-01-01T00:00:02Z"`,
		`{"request_id":"long","ts":"2026-01-01T00:00:03Z","error_message":"`+strings.Repeat("x", 100<<10)+`"}`,
		`{"request_id":"last","ts":"2026-01-01T00:00:04Z"}`,
	)
	if diff := cmp.Diff([]string{"last", "long", "first"}, requestIDs(t, NewReader(path))); diff != "" {
		t.Errorf("request ids, newest first (-want +got):\n%s", diff)
	}
}

func TestReaderFailsOnALineOverTheScanLimit(t *testing.T) {
	path := writeLines(t,
		`{"request_id":"first","ts":"2026-01-01T00:00:01Z"}`,
		`{"request_id":"huge","error_message":"`+strings.Repeat("x", 4<<20)+`"}`,
	)
	r := NewReader(path)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"Events":     func() error { _, err := r.Events(ctx, usage.EventQuery{}); return err },
		"Count":      func() error { _, err := r.Count(ctx, usage.EventQuery{}, 0); return err },
		"Summary":    func() error { _, err := r.Summary(ctx, usage.SummaryQuery{}); return err },
		"TimeSeries": func() error { _, err := r.TimeSeries(ctx, usage.TimeSeriesQuery{Interval: time.Hour}); return err },
		"Facet":      func() error { _, err := r.Facet(ctx, usage.FacetQuery{Dim: "model"}); return err },
	} {
		if err := call(); err == nil || !strings.HasPrefix(err.Error(), "usage/file.Reader: scan: ") {
			t.Errorf("%s: err = %v, want a scan error", name, err)
		}
	}
}

// A file that does not exist yet reads as empty: the relay may be queried before its first request.
func TestReaderMissingFileIsEmpty(t *testing.T) {
	r := NewReader(filepath.Join(t.TempDir(), "absent.jsonl"))
	ctx := context.Background()
	if evs, err := r.Events(ctx, usage.EventQuery{}); err != nil || len(evs) != 0 {
		t.Errorf("Events = %v, %v", evs, err)
	}
	if n, err := r.Count(ctx, usage.EventQuery{}, 0); err != nil || n != 0 {
		t.Errorf("Count = %d, %v", n, err)
	}
	if res, err := r.Summary(ctx, usage.SummaryQuery{}); err != nil || len(res.Rows) != 0 {
		t.Errorf("Summary = %+v, %v", res, err)
	}
	if res, err := r.TimeSeries(ctx, usage.TimeSeriesQuery{Interval: time.Minute}); err != nil || len(res.Rows) != 0 || res.Interval != "1m0s" {
		t.Errorf("TimeSeries = %+v, %v", res, err)
	}
	if vals, err := r.Facet(ctx, usage.FacetQuery{Dim: "model"}); err != nil || len(vals) != 0 {
		t.Errorf("Facet = %v, %v", vals, err)
	}
}

func TestReaderUnreadablePathFails(t *testing.T) {
	// A directory opens but cannot be scanned.
	_, err := NewReader(t.TempDir()).Events(context.Background(), usage.EventQuery{})
	if err == nil || !strings.HasPrefix(err.Error(), "usage/file.Reader: ") {
		t.Fatalf("err = %v, want a reader error", err)
	}
}

func TestReaderRejectsInvalidQueries(t *testing.T) {
	r := NewReader(writeFixture(t, nil))
	ctx := context.Background()
	for name, tc := range map[string]struct {
		call func() error
		want string
	}{
		"summary group_by":    {func() error { _, err := r.Summary(ctx, usage.SummaryQuery{GroupBy: "bogus"}); return err }, `usage.Summarize: invalid group_by "bogus"`},
		"timeseries interval": {func() error { _, err := r.TimeSeries(ctx, usage.TimeSeriesQuery{}); return err }, "usage.Bucketize: interval must be > 0"},
		"timeseries group_by": {func() error {
			_, err := r.TimeSeries(ctx, usage.TimeSeriesQuery{Interval: time.Hour, GroupBy: "bogus"})
			return err
		}, `usage.Bucketize: invalid group_by "bogus"`},
		"facet dim": {func() error { _, err := r.Facet(ctx, usage.FacetQuery{Dim: "bogus"}); return err }, `usage.TopValues: invalid dim "bogus"`},
	} {
		if err := tc.call(); err == nil || err.Error() != tc.want {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
