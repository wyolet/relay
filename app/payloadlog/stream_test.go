package payloadlog

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/lifecycle"
)

func TestStreamPayloadObserver(t *testing.T) {
	optedOut := optedIn("in")
	optedOut.PayloadLog = false
	clipped := fullRecord("in", "data: a\n\ndata")
	clipped.ResponseTruncated = true

	for _, tc := range []struct {
		name     string
		maxBytes int // -1: logging globally off
		lc       *lifecycle.Context
		frames   []string
		want     *Record
	}{
		{"frames joined by the separator the framer stripped", 0, optedIn("in"), []string{"data: a", "data: b"}, fullRecord("in", "data: a\n\ndata: b\n\n")},
		{"no frames", 0, optedIn("in"), nil, fullRecord("in", "")},
		// The frame that crosses the cap is kept whole in the buffer and clipped in the record; later ones are not buffered.
		{"stops buffering at the cap", 13, optedIn("in"), []string{"data: a", "data: b", "data: c"}, clipped},
		{"request never opted in", 0, optedOut, []string{"data: a"}, nil},
		{"logging globally off", -1, optedIn("in"), []string{"data: a"}, nil},
		{"no context", 0, nil, []string{"data: a"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := disabledCtrl()
			if tc.maxBytes >= 0 {
				c, _ = enabledCtrl(tc.maxBytes)
			}
			factory := NewStreamPayloadFactory(c)
			obs := factory.NewObserver(tc.lc)
			for _, f := range tc.frames {
				obs.Observe([]byte(f))
			}
			v, err := obs.Result()
			if err != nil || factory.Name() != Namespace {
				t.Fatalf("Result: %v (factory %q)", err, factory.Name())
			}
			if tc.want == nil {
				if v != nil {
					t.Fatalf("Result = %+v, want nothing captured", v)
				}
				return
			}
			if diff := cmp.Diff(tc.want, v.(*Record)); diff != "" {
				t.Fatalf("record (-want +got):\n%s", diff)
			}
		})
	}
}

// Once the cap is reached the observer holds no more of the stream.
func TestStreamPayloadObserver_BufferStopsGrowingAtTheCap(t *testing.T) {
	c, _ := enabledCtrl(8)
	obs := NewStreamPayloadFactory(c).NewObserver(optedIn("in")).(*streamPayloadObserver)
	for range 100 {
		obs.Observe([]byte("0123456789"))
	}
	if obs.buf.Len() != len("0123456789\n\n") {
		t.Fatalf("buffered %d bytes of a 1200-byte stream capped at 8, want only the frame that crossed the cap", obs.buf.Len())
	}
}
