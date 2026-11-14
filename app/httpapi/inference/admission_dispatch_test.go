package inference

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// At the in-flight cap the pre-flight abort is relay's retriable 503 with Retry-After, and no usage row: a flood must not turn into one event per refusal.
func TestDispatchShedAnswersOverloaded(t *testing.T) {
	adm := admissionAt(t, true)
	d := Deps{}
	outcomes := watchOutcomes(&d)
	d.Lifecycle.RegisterPreFlight(adm.PreFlight)
	d.Lifecycle.RegisterCollector(adm)

	w := httptest.NewRecorder()
	Dispatch(d, w, httptest.NewRequest(http.MethodPost, "/", nil), DispatchInput{Inbound: adapters.OpenAI, Body: []byte(`{"model":"m"}`), ModelName: "m"})
	assertOverloaded(t, w)
	select {
	case o := <-outcomes:
		t.Errorf("a shed request was finalized: %+v", o)
	case <-time.After(50 * time.Millisecond):
	}
}

// Any other pre-flight abort is relay's own failure: a 500 without Retry-After, recorded.
func TestDispatchNonShedPreFlightStays500(t *testing.T) {
	d := Deps{}
	outcomes := watchOutcomes(&d)
	d.Lifecycle.RegisterPreFlight(func(context.Context, *lifecycle.Context, *lifecycle.PreFlightEvent) error {
		return context.DeadlineExceeded
	})

	w := httptest.NewRecorder()
	Dispatch(d, w, httptest.NewRequest(http.MethodPost, "/", nil), DispatchInput{Inbound: adapters.OpenAI, Body: []byte(`{"model":"m"}`), ModelName: "m"})

	wantAnswer(t, w, http.StatusInternalServerError, httpapi.APIErrorBody{Type: "server_error", Code: "pre_flight_aborted", Message: "context deadline exceeded"}, map[string]string{"Retry-After": "", HeaderOrigin: "relay"})
	wantOutcome(t, outcomes, outcome{Kind: "pre_flight_aborted", Message: "context deadline exceeded"})
}
