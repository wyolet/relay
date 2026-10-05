package inference

import (
	"bytes"
	"strings"
	"testing"
)

func TestFrameDropWriterSplitWrites(t *testing.T) {
	stream := "data: a\n\ndata: DROP\r\n\r\ndata: b\n\ndata: DROP\n\ndata: [DONE]\n\ntrailing"
	want := "data: a\n\ndata: b\n\ndata: [DONE]\n\ntrailing"
	drop := func(f []byte) bool { return bytes.Contains(f, []byte("DROP")) }

	for size := 1; size <= len(stream); size++ {
		var out bytes.Buffer
		f := &frameDropWriter{w: &out, drop: drop}
		for i := 0; i < len(stream); i += size {
			end := min(i+size, len(stream))
			if n, err := f.Write([]byte(stream[i:end])); err != nil || n != end-i {
				t.Fatalf("size %d: Write = %d, %v", size, n, err)
			}
		}
		if err := f.writeHeld(); err != nil {
			t.Fatalf("size %d: writeHeld: %v", size, err)
		}
		if out.String() != want {
			t.Fatalf("size %d: got %q, want %q", size, out.String(), want)
		}
	}
}

func TestFrameDropWriterPassesNonSSEThrough(t *testing.T) {
	var out bytes.Buffer
	f := &frameDropWriter{w: &out, drop: func([]byte) bool { return true }}
	big := strings.Repeat("x", maxHeldFrameBytes+1)
	if _, err := f.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	if out.Len() != len(big) {
		t.Fatalf("passed through %d bytes, want %d", out.Len(), len(big))
	}
}
