package batch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A submission over the item cap is refused before anything is written, so
// the store and queue are never touched (nil here).
func TestSubmitRefusesMoreItemsThanTheCap(t *testing.T) {
	s := NewService(nil, nil, nil, nil, 2)
	_, err := s.Submit(context.Background(), &Caller{KeyHash: "k"}, "openai", [][]byte{[]byte(`{}`), []byte(`{}`), []byte(`{}`)})
	if !errors.Is(err, ErrTooManyItems) {
		t.Fatalf("err = %v, want ErrTooManyItems", err)
	}
}

func TestSubmitHandlerAnswers400OverTheCap(t *testing.T) {
	s := NewService(nil, nil, nil, func(context.Context) *Caller { return &Caller{KeyHash: "k"} }, 2)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"shape":"openai","requests":[{},{},{}]}`))
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "2") {
		t.Fatalf("error does not name the limit: %s", w.Body)
	}
}

func TestNewServiceDefaultsTheCap(t *testing.T) {
	if got := NewService(nil, nil, nil, nil, 0).maxItems; got != DefaultMaxItems {
		t.Fatalf("maxItems = %d, want %d", got, DefaultMaxItems)
	}
}
