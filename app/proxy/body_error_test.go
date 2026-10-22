package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
)

// refusingBody yields head, then fails with a BodyError, as a body reader that rejects the rest of a request does.
type refusingBody struct {
	head string
	sent bool
}

func (b *refusingBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, b.head), nil
	}
	return 0, &BodyError{Kind: "duplicate_model", Err: errors.New("refused")}
}

// A body refused mid-forward fails Run with the BodyError, over HTTP/1.1 and HTTP/2, the upstream never sees the body end, and the failure is recorded under the body's kind.
func TestRun_BodyErrorAbortsTheForward(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		name := "http1"
		if h2 {
			name = "http2"
		}
		t.Run(name, func(t *testing.T) {
			upstreamGot := make(chan error, 1)
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := io.ReadAll(r.Body)
				if h2 && r.ProtoMajor != 2 {
					err = errors.New("served over " + r.Proto)
				}
				upstreamGot <- err
				_, _ = w.Write([]byte(`{}`))
			}))
			srv.EnableHTTP2 = h2
			srv.StartTLS()
			defer srv.Close()

			kinds := make(chan string, 1)
			reg := lifecycle.New()
			reg.RegisterHook(lifecycle.HookFunc{HookName: "kind", Fn: func(_ *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
				kinds <- ev.ErrorKind
				return nil, nil
			}})
			p := New(nil, reg, nil)
			p.Client = srv.Client()
			head := `{"model":"a","messages":[`
			_, err := p.Run(context.Background(), &Request{
				Method:        http.MethodPost,
				Path:          "/v1/chat/completions",
				Body:          &refusingBody{head: head},
				ContentLength: int64(len(head) + 64),
				Headers:       http.Header{},
				HostBaseURL:   srv.URL,
				UpstreamAuth:  "Bearer k",
				Lifecycle:     lifecycle.NewContext("req", "proxy", time.Now()),
			})
			var be *BodyError
			if !errors.As(err, &be) || be.Kind != "duplicate_model" {
				t.Fatalf("Run err = %v, want the BodyError", err)
			}
			select {
			case got := <-upstreamGot:
				if got == nil || strings.HasPrefix(got.Error(), "served over") {
					t.Fatalf("upstream read the body to its end (%v)", got)
				}
			case <-time.After(5 * time.Second):
			}
			select {
			case k := <-kinds:
				if k != "duplicate_model" {
					t.Fatalf("failure kind = %q", k)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no failure event")
			}
		})
	}
}

func TestClassifyFailure_BodyError(t *testing.T) {
	err := &BodyError{Kind: "read_body", Err: io.ErrUnexpectedEOF}
	if kind, _ := classifyFailure(errors.Join(errors.New("proxy: upstream call"), err)); kind != "read_body" {
		t.Fatalf("kind = %q", kind)
	}
	if !strings.Contains(err.Error(), "unexpected EOF") || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("BodyError hides its cause: %v", err)
	}
}
