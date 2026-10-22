package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/wyolet/relay/app/proxy"
)

// scannedFrom scans prefixLen bytes of body up front, as the resolver does, and streams the rest from src.
func scannedFrom(t *testing.T, body []byte, prefixLen int, src io.Reader) *scannedBody {
	t.Helper()
	scan := &modelScanner{}
	prefix := body[:prefixLen]
	closeAt := scan.feed(prefix)
	if scan.err != nil {
		t.Fatalf("prefix rejected: %v", scan.err)
	}
	return newScannedBody(scan, prefix, closeAt, src, io.NopCloser(nil))
}

func TestScannedBody_ForwardsTheBodyUnchanged(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"{\"model\":\"x\"}"}],"max_tokens":5}  ` + "\n")
	for _, prefixLen := range []int{14, len(body) - 4, len(body)} {
		for _, src := range []func([]byte) io.Reader{
			func(b []byte) io.Reader { return bytes.NewReader(b) },
			func(b []byte) io.Reader { return iotest.OneByteReader(bytes.NewReader(b)) },
			func(b []byte) io.Reader { return iotest.DataErrReader(bytes.NewReader(b)) },
		} {
			b := scannedFrom(t, body, prefixLen, src(body[prefixLen:]))
			got, err := io.ReadAll(b)
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("prefix %d: read %q, %v", prefixLen, got, err)
			}
			if b.rejection() != nil {
				t.Fatalf("prefix %d: rejected a clean body", prefixLen)
			}
		}
	}
}

// The top-level close is held until the client's body ends: an upstream never holds a complete JSON body while bytes that could still be refused are unread.
func TestScannedBody_HoldsTheTailUntilTheBodyEnds(t *testing.T) {
	body := []byte(`{"model":"m","messages":[]}`)
	pr, pw := io.Pipe()
	b := scannedFrom(t, body, 12, pr)
	go func() { _, _ = pw.Write(body[12:]) }()

	var got []byte
	buf := make([]byte, 64)
	for len(got) < len(body)-1 {
		n, err := b.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		got = append(got, buf[:n]...)
	}
	if !bytes.Equal(got, body[:len(body)-1]) {
		t.Fatalf("released %q before the body ended", got)
	}
	released := make(chan []byte, 1)
	go func() {
		rest, _ := io.ReadAll(b)
		released <- rest
	}()
	select {
	case r := <-released:
		t.Fatalf("released %q while the client could still send", r)
	case <-time.After(50 * time.Millisecond):
	}
	_ = pw.Close()
	if rest := <-released; string(rest) != "}" {
		t.Fatalf("tail = %q, want the closing brace", rest)
	}
}

func TestScannedBody_Refusals(t *testing.T) {
	pad := strings.Repeat("a", 64<<10)
	cases := []struct {
		name string
		body string
		src  func(rest []byte) io.Reader
		code string
	}{
		{"duplicate late", `{"model":"a","messages":[{"content":"` + pad + `"}],"model":"b"}`, nil, "duplicate_model"},
		{"duplicate split across reads", `{"model":"a","x":"` + pad + `","mod` + `el":"b"}`, func(r []byte) io.Reader { return iotest.OneByteReader(bytes.NewReader(r)) }, "duplicate_model"},
		{"trailing value", `{"model":"a","x":"` + pad + `"} {"model":"b"}`, nil, "parse_error"},
		{"ends before the close", `{"model":"a","x":"` + pad + `"`, nil, "parse_error"},
		{"malformed tail", `{"model":"a","x":"` + pad + `",}`, nil, "parse_error"},
		{"client disconnect", `{"model":"a","x":"` + pad + `"}`, func(r []byte) io.Reader {
			return io.MultiReader(bytes.NewReader(r[:len(r)/2]), iotest.ErrReader(io.ErrUnexpectedEOF))
		}, "read_body"},
		{"over the body limit", `{"model":"a","x":"` + pad + `"}`, func(r []byte) io.Reader {
			return http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(bytes.NewReader(r)), int64(len(r)/2))
		}, "request_too_large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			rest := body[13:]
			var src io.Reader = bytes.NewReader(rest)
			if tc.src != nil {
				src = tc.src(rest)
			}
			b := scannedFrom(t, body, 13, src)
			got, err := io.ReadAll(b)
			var be *proxy.BodyError
			if !errors.As(err, &be) || be.Kind != tc.code {
				t.Fatalf("err = %v, want a %s BodyError", err, tc.code)
			}
			if rej := b.rejection(); rej == nil || rej.code != tc.code {
				t.Fatalf("rejection = %v", rej)
			}
			if json.Valid(got) {
				t.Fatalf("released a complete JSON body (%d bytes)", len(got))
			}
			if _, again := b.Read(make([]byte, 8)); again != err {
				t.Fatalf("read after refusal: %v, want the same refusal", again)
			}
		})
	}
}
