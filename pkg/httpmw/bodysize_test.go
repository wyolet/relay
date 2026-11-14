package httpmw_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/iotest"

	"github.com/wyolet/relay/pkg/httpmw"
)

// headerTrap fails the test on any use: the limit must not touch the response, whose headers the handler may be writing on another goroutine.
type headerTrap struct{ t *testing.T }

func (h headerTrap) Header() http.Header       { h.t.Fatal("limit touched the response headers"); return nil }
func (h headerTrap) Write([]byte) (int, error) { h.t.Fatal("limit wrote the response"); return 0, nil }
func (h headerTrap) WriteHeader(int)           { h.t.Fatal("limit wrote a status") }

type closeCounter struct {
	io.Reader
	closed int
}

func (c *closeCounter) Close() error { c.closed++; return nil }

// A body within the limit reads whole; a longer one yields exactly the limit and then *http.MaxBytesError, however the reads are sized.
func TestLimitBody(t *testing.T) {
	const limit = 1024
	for _, size := range []int{0, 1, limit - 1, limit, limit + 1, 2 * limit} {
		for _, chunked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d bytes, one byte per read %v", size, chunked), func(t *testing.T) {
				var src io.Reader = bytes.NewReader(bytes.Repeat([]byte("a"), size))
				if chunked {
					src = iotest.OneByteReader(src)
				}
				body := &closeCounter{Reader: src}
				var got []byte
				var err, again error
				httpmw.LimitBody(limit)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					if n, zeroErr := r.Body.Read(nil); n != 0 || zeroErr != nil {
						t.Errorf("empty read = %d, %v", n, zeroErr)
					}
					got, err = io.ReadAll(r.Body)
					_, again = r.Body.Read(make([]byte, 8))
					_ = r.Body.Close()
				})).ServeHTTP(headerTrap{t}, httptest.NewRequest(http.MethodPost, "/", body))

				var tooLarge *http.MaxBytesError
				if size <= limit {
					if len(got) != size || err != nil || again != io.EOF {
						t.Fatalf("read %d bytes, err %v, then %v; want all %d, nil, EOF", len(got), err, again, size)
					}
				} else if len(got) != limit || !errors.As(err, &tooLarge) || tooLarge.Limit != limit || again != err || !httpmw.IsBodyTooLargeError(err) {
					t.Fatalf("read %d bytes, err %v, then %v; want %d bytes and a sticky MaxBytesError for limit %d", len(got), err, again, limit, limit)
				}
				if body.closed != 1 {
					t.Errorf("underlying body closed %d times, want 1", body.closed)
				}
			})
		}
	}
}

func TestIsBodyTooLargeError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("some other error"), false},
		{io.ErrUnexpectedEOF, false},
		{&http.MaxBytesError{Limit: 10}, true},
		{fmt.Errorf("read body: %w", &http.MaxBytesError{Limit: 10}), true},
	} {
		if got := httpmw.IsBodyTooLargeError(tc.err); got != tc.want {
			t.Errorf("IsBodyTooLargeError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
