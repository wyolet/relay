package pipeline_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pipeline"
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

func (c usageCapture) next(t *testing.T) usagelog.Event {
	t.Helper()
	select {
	case ev := <-c:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no usage event emitted")
		return usagelog.Event{}
	}
}

// usageRegistry wires the usage producers the composition root registers, plus the capture.
func usageRegistry() (*lifecycle.Registry, usageCapture) {
	events := make(usageCapture, 4)
	reg := lifecycle.New()
	reg.RegisterHook(usagelog.NewUsageHook(nil, ""))
	reg.RegisterStreamObserver(usagelog.NewStreamUsageFactory(nil, ""))
	reg.RegisterCollector(events)
	return reg, events
}

func errorResp(status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"rejected"}}`)),
	}
}

// An error status the pipeline hands back to the caller is stored with that status and the upstream-error kind, buffered and streamed alike.
func TestRun_PassedThroughErrorStatusIsAnUpstreamError(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusServiceUnavailable} {
		for _, stream := range []bool{false, true} {
			reg, events := usageRegistry()
			p := newPipeline()
			p.Lifecycle = reg

			lc := lifecycle.NewContext("req", "pipeline", time.Now())
			lc.Translator = identityTranslator{}
			res, err := p.Run(context.Background(), &pipeline.Request{
				// The default fake adapter retries nothing, so every status is passed through.
				Adapter: &fakeAdapter{callFn: func(context.Context, string, string, []byte, http.Header) (*http.Response, error) {
					return errorResp(status), nil
				}},
				Keys:      []*hostkey.HostKey{makeKey("h", "sk")},
				Policy:    makePolicy(),
				Stream:    stream,
				Lifecycle: lc,
			})
			if err != nil {
				t.Fatalf("status %d stream=%v: Run: %v", status, stream, err)
			}
			if res.Status != status {
				t.Fatalf("status %d stream=%v: result status %d", status, stream, res.Status)
			}
			drainResult(t, res)

			ev := events.next(t)
			if ev.Status != status || ev.ErrorKind != usage.ErrorKindUpstream || ev.Streamed != stream {
				t.Errorf("status %d stream=%v: event status=%d kind=%q streamed=%v", status, stream, ev.Status, ev.ErrorKind, ev.Streamed)
			}
		}
	}
}

// A retryable error status that exhausts the keys reaches the usage log through the failure path with the same kind and status.
func TestRun_ExhaustedErrorStatusIsAnUpstreamError(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		reg, events := usageRegistry()
		p := newPipeline()
		p.Lifecycle = reg

		_, err := p.Run(context.Background(), &pipeline.Request{
			Adapter: &fakeAdapter{
				callFn: func(context.Context, string, string, []byte, http.Header) (*http.Response, error) {
					return errorResp(status), nil
				},
				retryFn: func(*http.Response) (bool, keypool.FailureKind, time.Duration) {
					return true, keypool.FailureServerError, 0
				},
			},
			Keys:      []*hostkey.HostKey{makeKey("h", "sk")},
			Policy:    makePolicy(),
			Lifecycle: lifecycle.NewContext("req", "pipeline", time.Now()),
		})
		if err == nil {
			t.Fatalf("status %d: Run succeeded, want the exhausted-keys error", status)
		}
		ev := events.next(t)
		if ev.Status != status || ev.ErrorKind != usage.ErrorKindUpstream {
			t.Errorf("status %d: event status=%d kind=%q", status, ev.Status, ev.ErrorKind)
		}
	}
}
