package inference

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/usage"
	"github.com/wyolet/relay/sdk/adapters/openai"
	v1 "github.com/wyolet/relay/sdk/v1"
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

const upstreamErrorBody = `{"error":{"message":"rejected","type":"invalid_request_error"}}`

// upstreamErrorDeps wires a dispatch whose one host answers every call with status, through the real retry classification and the usage producers the composition root registers.
func upstreamErrorDeps(t *testing.T, status int) (Deps, *Principal, usageCapture) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(upstreamErrorBody))
	}))
	t.Cleanup(up.Close)

	cat, pr := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	pointHostAt(t, cat, up.URL)

	d := buildRunnableDeps(t, cat)
	specs := adapter.NewRegistry(
		(&adapter.Spec{
			Name:          adapters.OpenAI,
			DefaultPath:   "/v1/chat/completions",
			Auth:          adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
			Translator:    openai.CCTranslator{},
			ExtractTokens: openai.ExtractTokens,
		}).Build(),
		(&adapter.Spec{Name: adapters.Canonical, Translator: v1.IdentityTranslator{}}).Build(),
	)
	useSpecs(&d, specs)

	events := make(usageCapture, 4)
	reg := lifecycle.New()
	reg.RegisterHook(usagelog.NewUsageHook(nil, ""))
	reg.RegisterStreamObserver(usagelog.NewStreamUsageFactory(nil, ""))
	reg.RegisterCollector(events)
	d.Lifecycle = reg
	d.Pipeline.Lifecycle = reg
	return d, pr, events
}

// A 400 is passed through and a 500 is returned after the only key failed; both reach the caller with the provider's status and are stored with it and the upstream-error kind, on the same-shape and the translated path, buffered and streamed.
func TestDispatch_UpstreamErrorStatusIsStoredAsUpstreamError(t *testing.T) {
	bodies := map[adapters.Name]string{
		adapters.OpenAI:    `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`,
		adapters.Canonical: `{"model":"test-model","input":"hi"}`,
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		for inbound, body := range bodies {
			for _, stream := range []bool{false, true} {
				name := fmt.Sprintf("%d %s stream=%v", status, inbound, stream)
				d, pr, events := upstreamErrorDeps(t, status)

				w := dispatch(d, pr, DispatchInput{Inbound: inbound, Body: []byte(body), ModelName: "test-model", Stream: stream})
				if w.Code != status || w.Body.String() != upstreamErrorBody {
					t.Fatalf("%s: caller got %d %s, want the provider's body verbatim", name, w.Code, w.Body)
				}

				ev := events.next(t)
				// An upstream's own error is not a relay refusal.
				if ev.Status != status || ev.ErrorKind != usage.ErrorKindUpstream || ev.Refusal != "" {
					t.Errorf("%s: event status=%d kind=%q refusal=%q", name, ev.Status, ev.ErrorKind, ev.Refusal)
				}
			}
		}
	}
}

// A WebSocket frame runs the same dispatch, so its provider errors are stored the same way.
func TestWSFrame_UpstreamErrorStatusIsStoredAsUpstreamError(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		d, pr, events := upstreamErrorDeps(t, status)
		conn, ctx := wsConn(t, d, pr)
		for _, stream := range []bool{false, true} {
			id := fmt.Sprintf("f-%d-%v", status, stream)
			got, body := wsExchange(t, ctx, conn, id, fmt.Sprintf(`{"model":"test-model","input":"hi","stream":%v}`, stream))
			if got != status || body != upstreamErrorBody {
				t.Fatalf("%s: frame answered %d %s, want the provider's %d body", id, got, body, status)
			}
			if ev := events.next(t); ev.Status != status || ev.ErrorKind != usage.ErrorKindUpstream || ev.Refusal != "" {
				t.Errorf("%s: event status=%d kind=%q refusal=%q", id, ev.Status, ev.ErrorKind, ev.Refusal)
			}
		}
	}
}
