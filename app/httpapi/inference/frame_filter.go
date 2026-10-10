package inference

import (
	"io"
	"net/http"

	v1 "github.com/wyolet/relay/sdk/v1"
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
	// droppedCR: a dropped frame ended the held bytes on a CR, so an LF opening the next Write is the rest of its CRLF.
	droppedCR bool
}

func (f *frameDropWriter) Write(p []byte) (int, error) {
	f.held = append(f.held, p...)
	if f.droppedCR && len(f.held) > 0 && f.held[0] == '\n' {
		f.held = f.held[:copy(f.held, f.held[1:])]
	}
	f.droppedCR = false
	kept, pos := 0, 0 // f.held[kept:pos] is complete frames not yet written
	for {
		n, frame, _ := v1.SplitSSEFrames(f.held[pos:], false)
		if n == 0 {
			break
		}
		end := pos + n
		if frame != nil && f.drop(v1.NormalizeSSELineEnds(frame)) {
			// Blank lines before the frame end the previous one; they stay.
			start := pos
			for f.held[start] == '\n' || f.held[start] == '\r' {
				start++
			}
			if err := f.write(f.held[kept:start]); err != nil {
				return 0, err
			}
			kept = end
			f.droppedCR = end == len(f.held) && f.held[end-1] == '\r'
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
