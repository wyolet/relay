package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/usage"
)

// usageCapture is a lifecycle.Collector handing each finalized request's usage event to the test.
type usageCapture chan usagelog.Event

func (c usageCapture) Collect(lc *lifecycle.Context) {
	if v, ok := lc.Collected(usagelog.Namespace); ok {
		c <- *v.(*usagelog.Event)
	}
}

// A proxied request the upstream answered with an error status is stored with that status and the upstream-error kind, whether the answer was JSON or an event stream.
func TestRun_ForwardedErrorStatusIsAnUpstreamError(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusServiceUnavailable} {
		for _, contentType := range []string{"application/json", "text/event-stream"} {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"rejected"}}`))
			}))

			events := make(usageCapture, 1)
			reg := lifecycle.New()
			reg.RegisterHook(usagelog.NewUsageHook(nil, ""))
			reg.RegisterStreamObserver(usagelog.NewStreamUsageFactory(nil, ""))
			reg.RegisterCollector(events)
			p := New(nil, reg, nil)
			p.Client = upstream.Client()

			res, err := p.Run(context.Background(), &Request{
				Method:       http.MethodPost,
				Path:         "/v1/chat/completions",
				Body:         strings.NewReader(`{}`),
				Headers:      http.Header{},
				HostBaseURL:  upstream.URL,
				UpstreamAuth: "Bearer upstream-token",
				Lifecycle:    lifecycle.NewContext("req", "proxy", time.Now()),
			})
			if err != nil {
				t.Fatalf("status %d %s: Run: %v", status, contentType, err)
			}
			if res.Status != status {
				t.Fatalf("status %d %s: result status %d", status, contentType, res.Status)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()

			streamed := contentType == "text/event-stream"
			select {
			case ev := <-events:
				if ev.Status != status || ev.ErrorKind != usage.ErrorKindUpstream || ev.Streamed != streamed {
					t.Errorf("status %d %s: event status=%d kind=%q streamed=%v", status, contentType, ev.Status, ev.ErrorKind, ev.Streamed)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("status %d %s: no usage event emitted", status, contentType)
			}
			upstream.Close()
		}
	}
}
