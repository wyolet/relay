package v1

import (
	"bytes"
	"testing"
)

type keepAliveTranslator struct{ IdentityTranslator }

func (keepAliveTranslator) KeepAliveFrame() []byte { return []byte("event: ping\ndata: {}\n\n") }

func TestKeepAliveFrameFor_DeclaredFrame(t *testing.T) {
	got := KeepAliveFrameFor(keepAliveTranslator{})
	if !bytes.Equal(got, []byte("event: ping\ndata: {}\n\n")) {
		t.Fatalf("frame = %q, want the translator's own", got)
	}
}

func TestKeepAliveFrameFor_FallsBackToComment(t *testing.T) {
	got := KeepAliveFrameFor(IdentityTranslator{})
	if !bytes.Equal(got, DefaultKeepAliveFrame) {
		t.Fatalf("frame = %q, want the default SSE comment", got)
	}
	// An SSE comment line: no field name, so every conforming consumer drops it.
	if !bytes.HasPrefix(got, []byte(":")) || !bytes.HasSuffix(got, []byte("\n\n")) {
		t.Fatalf("default frame %q is not a terminated SSE comment", got)
	}
}
