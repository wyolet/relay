package httpmw_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/pkg/httpmw"
)

func TestLimitBodyUnderLimit(t *testing.T) {
	limit := int64(1024)
	body := bytes.Repeat([]byte("a"), 512)

	handler := httpmw.LimitBody(limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestLimitBodyOverLimit(t *testing.T) {
	limit := int64(1024)
	body := bytes.Repeat([]byte("a"), 2048)

	handler := httpmw.LimitBody(limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if httpmw.IsBodyTooLargeError(err) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"message": "request body too large",
					"type":    "invalid_request_error",
					"code":    "request_too_large",
				},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
}

func TestLimitBodyExactLimit(t *testing.T) {
	limit := int64(1024)
	body := bytes.Repeat([]byte("a"), 1024)

	handler := httpmw.LimitBody(limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestIsBodyTooLargeError(t *testing.T) {
	if httpmw.IsBodyTooLargeError(nil) {
		t.Fatal("expected false for nil")
	}
	if httpmw.IsBodyTooLargeError(errors.New("some other error")) {
		t.Fatal("expected false for generic error")
	}

	// Trigger a real MaxBytesError via MaxBytesReader.
	body := bytes.Repeat([]byte("a"), 100)
	r := io.NopCloser(bytes.NewReader(body))
	rec := httptest.NewRecorder()
	limited := http.MaxBytesReader(rec, r, 10)
	_, err := io.ReadAll(limited)
	if !httpmw.IsBodyTooLargeError(err) {
		t.Fatalf("expected true for *http.MaxBytesError, got %T: %v", err, err)
	}
}
