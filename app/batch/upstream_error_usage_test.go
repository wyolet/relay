package batch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/usage"
)

// usageCapture is a lifecycle.Collector handing each finalized item's usage event to the test.
type usageCapture chan usagelog.Event

func (c usageCapture) Collect(lc *lifecycle.Context) {
	if v, ok := lc.Collected(usagelog.Namespace); ok {
		c <- *v.(*usagelog.Event)
	}
}

// A batch item the provider answered with an error status is stored with that status and the upstream-error kind: a 400 comes back as the item's result, a 500 fails the item after its only key.
func TestRun_UpstreamErrorStatusIsStoredAsUpstreamError(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"rejected"}}`))
		}))

		f := runnerFixture(t, up.URL)
		events := make(usageCapture, 1)
		reg := lifecycle.New()
		reg.RegisterHook(usagelog.NewUsageHook(nil, ""))
		reg.RegisterCollector(events)
		f.rn.Pipeline.Lifecycle = reg

		got, _, err := f.rn.Run(context.Background(), "item", f.key.Spec.KeyHash, TokenClaims{}, f.keyAttr(), adapters.OpenAI, []byte(`{"model":"test-model"}`))
		if status == http.StatusBadRequest && (err != nil || got != status) {
			t.Fatalf("status %d: Run = %d, %v; want the status as the item result", status, got, err)
		}
		if status == http.StatusInternalServerError && err == nil {
			t.Fatalf("status %d: Run succeeded, want the exhausted-keys error", status)
		}

		select {
		case ev := <-events:
			if ev.Source != "batch" || ev.Status != status || ev.ErrorKind != usage.ErrorKindUpstream {
				t.Errorf("status %d: event source=%q status=%d kind=%q", status, ev.Source, ev.Status, ev.ErrorKind)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("status %d: no usage event emitted", status)
		}
		up.Close()
	}
}
