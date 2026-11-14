package lifecycle

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
)

// Every mark is the time elapsed since the request started, read off a fake clock.
func TestTimingMarks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lc := NewContext("r", "test", time.Now())
		step := func() { time.Sleep(10 * time.Millisecond) }

		step()
		lc.MarkUpstreamStart() // a first attempt
		step()
		lc.MarkUpstreamStart() // the attempt that is served: 20ms
		step()
		lc.MarkReasoningStart() // 30ms
		lc.MarkReasoningEnd()
		step()
		lc.MarkReasoningStart() // only the first start counts
		lc.MarkReasoningEnd()   // the last end counts: 40ms
		step()
		lc.MarkEnd() // 50ms

		want := Timing{
			Start:     lc.Timing.Start,
			Upstream:  UpstreamTiming{Start: 20 * time.Millisecond},
			Reasoning: ReasoningTiming{Start: 30 * time.Millisecond, End: 40 * time.Millisecond},
			End:       50 * time.Millisecond,
		}
		if diff := cmp.Diff(want, lc.Timing); diff != "" {
			t.Errorf("Timing (-want +got):\n%s", diff)
		}
	})

	var none *Context
	none.MarkUpstreamStart()
	none.MarkReasoningStart()
	none.MarkReasoningEnd()
	none.MarkEnd()
	r := strings.NewReader("x")
	if got := none.FirstByteReader(r); got != io.Reader(r) {
		t.Error("FirstByteReader on a nil Context wrapped the reader")
	}
}

// slowReader waits before each read, then serves the next of its reads; a nil entry is an empty read.
type slowReader struct {
	wait  time.Duration
	reads []string
}

func (s *slowReader) Read(p []byte) (int, error) {
	time.Sleep(s.wait)
	if len(s.reads) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.reads[0])
	s.reads = s.reads[1:]
	return n, nil
}

// The first byte stamps the response start, not an empty read before it; EOF stamps the response end.
func TestFirstByteReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lc := NewContext("r", "test", time.Now())
		body := lc.FirstByteReader(&slowReader{wait: 10 * time.Millisecond, reads: []string{"", "he", "llo"}})
		got, err := io.ReadAll(body)
		if err != nil || string(got) != "hello" {
			t.Fatalf("read %q, %v", got, err)
		}
		want := UpstreamTiming{ResponseStart: 20 * time.Millisecond, ResponseEnd: 40 * time.Millisecond}
		if diff := cmp.Diff(want, lc.Timing.Upstream); diff != "" {
			t.Errorf("upstream timing (-want +got):\n%s", diff)
		}
	})

	// A body that fails mid-stream never stamps the end mark.
	synctest.Test(t, func(t *testing.T) {
		lc := NewContext("r", "test", time.Now())
		time.Sleep(5 * time.Millisecond)
		body := lc.FirstByteReader(iotest.TimeoutReader(strings.NewReader("hello")))
		buf := make([]byte, 2)
		if n, err := body.Read(buf); n != 2 || err != nil {
			t.Fatalf("first read = %d, %v", n, err)
		}
		if _, err := body.Read(buf); err != iotest.ErrTimeout {
			t.Fatalf("second read: err = %v, want ErrTimeout", err)
		}
		if diff := cmp.Diff(UpstreamTiming{ResponseStart: 5 * time.Millisecond}, lc.Timing.Upstream); diff != "" {
			t.Errorf("upstream timing (-want +got):\n%s", diff)
		}
	})
}
