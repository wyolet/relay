package v1

import (
	"bufio"
	"io"
	"reflect"
	"strings"
	"testing"
)

// chunkReader returns its input in reads of at most size bytes.
type chunkReader struct {
	s    string
	size int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.s == "" {
		return 0, io.EOF
	}
	n := min(r.size, len(p), len(r.s))
	copy(p, r.s[:n])
	r.s = r.s[n:]
	return n, nil
}

// partsReader returns each part as one read.
type partsReader struct{ parts []string }

func (r *partsReader) Read(p []byte) (int, error) {
	if len(r.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.parts[0])
	if r.parts[0] = r.parts[0][n:]; r.parts[0] == "" {
		r.parts = r.parts[1:]
	}
	return n, nil
}

func scanFrames(t *testing.T, r io.Reader) []string {
	t.Helper()
	sc := bufio.NewScanner(r)
	sc.Split(SplitSSEFrames)
	var frames []string
	for sc.Scan() {
		frames = append(frames, string(NormalizeSSELineEnds(sc.Bytes())))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return frames
}

func TestSplitSSEFrames(t *testing.T) {
	cases := []struct {
		name, stream string
		want         []string
	}{
		{"LF", "event: x\ndata: a\n\ndata: b\n\n", []string{"event: x\ndata: a", "data: b"}},
		{"CRLF", "event: x\r\ndata: a\r\n\r\ndata: b\r\n\r\n", []string{"event: x\ndata: a", "data: b"}},
		{"CR", "event: x\rdata: a\r\rdata: b\r\r", []string{"event: x\ndata: a", "data: b"}},
		{"mixed blank lines", "data: a\r\n\ndata: b\n\r\ndata: c\r\r\ndata: d\n\rdata: e\n\n", []string{"data: a", "data: b", "data: c", "data: d", "data: e"}},
		{"mixed endings inside a frame", "event: e\rid: 1\r\ndata: f\n\n", []string{"event: e\nid: 1\ndata: f"}},
		{"extra blank lines", "\r\ndata: a\n\n\n\ndata: b\r\n\r\n\r\n\r\r", []string{"data: a", "data: b"}},
		{"trailing frame without terminator", "data: a\n\ndata: b", []string{"data: a", "data: b"}},
		{"trailing CRLF frame ended by one line end", "data: a\r\n\r\ndata: b\r\n", []string{"data: a", "data: b"}},
		{"trailing CR at EOF", "data: a\rdata: b\r", []string{"data: a\ndata: b"}},
		{"blank lines only", "\r\n\r\n\n", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for size := 1; size <= len(c.stream); size++ {
				if got := scanFrames(t, &chunkReader{s: c.stream, size: size}); !reflect.DeepEqual(got, c.want) {
					t.Fatalf("read size %d: frames %q, want %q", size, got, c.want)
				}
			}
		})
	}
}

func TestSplitSSEFramesReadSplitAtCRLF(t *testing.T) {
	// A CR ending one read may be the first half of a CRLF: it must not end the line on its own.
	got := scanFrames(t, &partsReader{parts: []string{"data: a\r", "\nid: 1\r\n\r\n"}})
	if want := []string{"data: a\nid: 1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("frames %q, want %q", got, want)
	}
	got = scanFrames(t, &partsReader{parts: []string{"data: a\r\n\r", "\ndata: b\r\n\r\n"}})
	if want := []string{"data: a", "data: b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("frames %q, want %q", got, want)
	}
}

func TestSplitSSEFramesDecidesAtTrailingCR(t *testing.T) {
	if n, tok, _ := SplitSSEFrames([]byte("data: a\r"), false); n != 0 || tok != nil {
		t.Fatalf("lone trailing CR: advance %d token %q, want to wait for more data", n, tok)
	}
	// The blank line is complete at its CR; the frame must not wait for the next read.
	if n, tok, _ := SplitSSEFrames([]byte("data: a\r\n\r"), false); n != 10 || string(tok) != "data: a" {
		t.Fatalf("CRLF then CR: advance %d token %q, want 10 \"data: a\"", n, tok)
	}
}

func TestSplitSSEFramesLFAllocFree(t *testing.T) {
	data := []byte(strings.Repeat("data: {\"x\":1}\n", 3) + "\n")
	allocs := testing.AllocsPerRun(100, func() {
		_, tok, _ := SplitSSEFrames(data, false)
		_ = NormalizeSSELineEnds(tok)
	})
	if allocs != 0 {
		t.Fatalf("allocs per LF frame = %v, want 0", allocs)
	}
}
