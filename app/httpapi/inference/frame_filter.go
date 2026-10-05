package inference

import (
	"bytes"
	"io"
	"net/http"
)

// maxHeldFrameBytes bounds the partial frame frameDropWriter holds back. Output
// that runs past it without a frame separator is not SSE and is passed through
// unfiltered.
const maxHeldFrameBytes = 1 << 20

// frameDropWriter forwards an SSE stream frame by frame, leaving out the
// frames drop reports true for. Complete frames are written as they arrive;
// only a trailing partial frame is held back. Call writeHeld after the last
// Write.
type frameDropWriter struct {
	w    io.Writer
	drop func(frame []byte) bool
	held []byte
}

func (f *frameDropWriter) Write(p []byte) (int, error) {
	f.held = append(f.held, p...)
	kept, pos := 0, 0 // f.held[kept:pos] is complete frames not yet written
	for {
		i, sep := nextFrameEnd(f.held[pos:])
		if i < 0 {
			break
		}
		end := pos + i + sep
		if f.drop(f.held[pos : pos+i]) {
			if err := f.write(f.held[kept:pos]); err != nil {
				return 0, err
			}
			kept = end
		}
		pos = end
	}
	if err := f.write(f.held[kept:pos]); err != nil {
		return 0, err
	}
	f.held = f.held[:copy(f.held, f.held[pos:])]
	if len(f.held) > maxHeldFrameBytes {
		return len(p), f.writeHeld()
	}
	return len(p), nil
}

// writeHeld writes the held partial frame, if any.
func (f *frameDropWriter) writeHeld() error {
	err := f.write(f.held)
	f.held = f.held[:0]
	return err
}

func (f *frameDropWriter) write(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	_, err := f.w.Write(b)
	return err
}

// Flush lets streamCopy flush through the filter.
func (f *frameDropWriter) Flush() {
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
}

// nextFrameEnd returns the length of the first SSE frame in b and the length
// of its separator ("\n\n" or "\r\n\r\n"), or -1 when b holds no full frame.
func nextFrameEnd(b []byte) (int, int) {
	i, sep := bytes.Index(b, []byte("\n\n")), 2
	if j := bytes.Index(b, []byte("\r\n\r\n")); j >= 0 && (i < 0 || j < i) {
		i, sep = j, 4
	}
	return i, sep
}
