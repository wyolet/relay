package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// recordingFactory hands every session one observer that records its frames; resultErr and panicOn make it misbehave.
type recordingFactory struct {
	name      string
	obs       *recordingObserver
	resultErr error
	panicOn   string // "observe" or "result"
}

func (f *recordingFactory) Name() string { return f.name }

func (f *recordingFactory) NewObserver(*Context) StreamObserver {
	f.obs = &recordingObserver{f: f}
	return f.obs
}

type recordingObserver struct {
	f       *recordingFactory
	frames  []string
	results int
}

func (o *recordingObserver) Observe(frame []byte) {
	o.frames = append(o.frames, string(frame))
	if o.f.panicOn == "observe" {
		panic("intentional in test")
	}
}

func (o *recordingObserver) Result() (any, error) {
	o.results++
	if o.f.panicOn == "result" {
		panic("intentional in test")
	}
	return len(o.frames), o.f.resultErr
}

func newRecordedSession(t *testing.T) (*StreamSession, *recordingFactory) {
	t.Helper()
	r := New()
	f := &recordingFactory{name: "rec"}
	r.RegisterStreamObserver(f)
	sess := r.NewStreamSession(NewContext("r", "test", time.Now()))
	if sess == nil {
		t.Fatal("no session for a registered stream observer")
	}
	return sess, f
}

// Raw upstream bytes are reframed on the SSE blank line whatever the write boundaries and line endings; observers get whole frames with LF line ends, and a trailing partial frame at Finish.
func TestStreamSessionWriteReframes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		writes []string
		want   []string
	}{
		{"frames split mid-frame across writes, unterminated tail",
			[]string{"event: a\ndata: 1\n\nev", "ent: b\ndata: 2\n\ndata: tail-no-sep"},
			[]string{"event: a\ndata: 1", "event: b\ndata: 2", "data: tail-no-sep"}},
		{"CRLF", []string{"event: a\r\ndata: 1\r\n\r\nevent: b\r\ndata: 2\r\n\r\n"}, []string{"event: a\ndata: 1", "event: b\ndata: 2"}},
		{"CR-only and mixed endings, one byte per write",
			strings.Split("event: a\rdata: 1\r\revent: b\r\ndata: 2\n\r\ndata: 3\r\r\n", ""),
			[]string{"event: a\ndata: 1", "event: b\ndata: 2", "data: 3"}},
		{"blank frames are dropped", []string{"\n\n\n\ndata: 1\n\n  \n\n", "\n\n"}, []string{"data: 1"}},
		{"nothing written", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, f := newRecordedSession(t)
			for _, w := range tc.writes {
				if n, err := sess.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write(%q) = %d, %v", w, n, err)
				}
			}
			sess.Finish()
			if diff := cmp.Diff(tc.want, f.obs.frames); diff != "" {
				t.Errorf("frames observed (-want +got):\n%s", diff)
			}
			if sess.Frames() != len(tc.want) {
				t.Errorf("Frames() = %d, want %d", sess.Frames(), len(tc.want))
			}
		})
	}
}

// Input with no separator never buffers more than maxPartialFrameBytes: the overflowing buffer is fed as one frame and nothing is lost.
func TestStreamSessionBoundsThePartialFrame(t *testing.T) {
	sess, f := newRecordedSession(t)
	chunk := strings.Repeat("x", 256<<10)
	for range 8 { // 2 MiB
		if _, err := sess.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if len(sess.buf) > maxPartialFrameBytes {
			t.Fatalf("partial buffer grew to %d, cap is %d", len(sess.buf), maxPartialFrameBytes)
		}
	}
	sess.Finish()
	var sizes []int
	for _, frame := range f.obs.frames {
		sizes = append(sizes, len(frame))
	}
	// The fifth chunk crosses the cap and flushes 1.25 MiB; Finish flushes the last three.
	if diff := cmp.Diff([]int{1280 << 10, 768 << 10}, sizes); diff != "" {
		t.Errorf("frame sizes (-want +got):\n%s", diff)
	}
}

// Finish attaches each observer's result, marks the request filled so post-flight hooks do not run again, and does all of it once.
func TestStreamSessionFinish(t *testing.T) {
	r := New()
	good := &recordingFactory{name: "good"}
	failing := &recordingFactory{name: "failing", resultErr: errors.New("no usage")}
	panicsOnResult := &recordingFactory{name: "panics-on-result", panicOn: "result"}
	panicsOnObserve := &recordingFactory{name: "panics-on-observe", panicOn: "observe"}
	last := &recordingFactory{name: "last"}
	for _, f := range []*recordingFactory{good, failing, panicsOnResult, panicsOnObserve, last} {
		r.RegisterStreamObserver(f)
	}
	hookRan := false
	r.RegisterHook(hookFn("hook", func(*Context, *PostFlightEvent) (any, error) {
		hookRan = true
		return "from hook", nil
	}))
	lc := NewContext("r", "test", time.Now())
	sess := r.NewStreamSession(lc)

	if _, ok := lc.StreamTokens(); ok {
		t.Fatal("stream tokens reported before Finish")
	}
	sess.Observe([]byte("a"))
	sess.Observe([]byte("b"))
	sess.Finish()
	sess.Finish()
	r.Finalize(context.Background(), lc, &PostFlightEvent{})

	// An observer that panicked on a frame keeps receiving frames and still reports.
	want := map[string]any{"good": 2, "panics-on-observe": 2, "last": 2}
	if diff := cmp.Diff(want, collected(lc, "good", "failing", "panics-on-result", "panics-on-observe", "last", "hook")); diff != "" {
		t.Errorf("collected after Finish and Finalize (-want +got):\n%s", diff)
	}
	if hookRan {
		t.Error("Finalize ran the hooks of a request its stream session already filled")
	}
	for _, f := range []*recordingFactory{good, failing, panicsOnResult, panicsOnObserve, last} {
		if f.obs.results != 1 {
			t.Errorf("%s: Result called %d times, want 1", f.name, f.obs.results)
		}
	}
	// No translator, so no tokens; "extracted, none" still differs from "not extracted".
	if tokens, ok := lc.StreamTokens(); !ok || len(tokens) != 0 {
		t.Errorf("StreamTokens = %v, %v; want none, extracted", tokens, ok)
	}
	if _, ok := lc.StreamSummary(); !ok {
		t.Error("no stream summary after Finish")
	}
}

func TestStreamSessionNilIsInert(t *testing.T) {
	var sess *StreamSession
	if n, err := sess.Write([]byte("data: 1\n\n")); n != 9 || err != nil {
		t.Errorf("Write on a nil session = %d, %v; want 9, nil", n, err)
	}
	sess.Observe([]byte("x"))
	sess.Finish()
	if sess.Frames() != 0 {
		t.Errorf("Frames() = %d", sess.Frames())
	}
	r := New()
	r.RegisterStreamObserver(&recordingFactory{name: "rec"})
	if r.NewStreamSession(nil) != nil {
		t.Error("a session for a nil Context")
	}
}
