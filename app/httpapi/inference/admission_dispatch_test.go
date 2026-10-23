package inference

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// TestDispatchShedAnswersOverloaded verifies that when the in-flight cap is reached the
// pre-flight abort is mapped to relay's retriable 503 with a Retry-After header —
// not the generic 500 other pre-flight errors get.
func TestDispatchShedAnswersOverloaded(t *testing.T) {
	adm := admissionAt(t, true)
	reg := lifecycle.New()
	reg.RegisterPreFlight(adm.PreFlight)
	reg.RegisterCollector(adm)

	body := []byte(`{"model":"m","stream":false}`)
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewReader(body))
	w := httptest.NewRecorder()
	Dispatch(Deps{Lifecycle: reg}, w, r, DispatchInput{Inbound: adapters.OpenAI, Body: body, ModelName: "m"})
	assertOverloaded(t, w)
}

// TestDispatchNonShedPreFlightStays500 confirms the shed mapping is type-aware:
// a different pre-flight abort still returns the generic 500, not the overload answer.
func TestDispatchNonShedPreFlightStays500(t *testing.T) {
	reg := lifecycle.New()
	reg.RegisterPreFlight(func(context.Context, *lifecycle.Context, *lifecycle.PreFlightEvent) error {
		return context.DeadlineExceeded // any non-shed pre-flight error
	})

	d := Deps{Lifecycle: reg}
	body := []byte(`{"model":"m"}`)
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewReader(body))
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{Inbound: adapters.OpenAI, Body: body, ModelName: "m"})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want empty on a non-shed abort", got)
	}
}
