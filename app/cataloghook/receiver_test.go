package cataloghook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, ReleasePath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestReceiverEchoesTheChallenge(t *testing.T) {
	h := ReleaseHandler("v1alpha3", func() { t.Fatal("a challenge must not start a check") })
	rec := post(t, h, `{"type":"challenge","challenge":"0123456789abcdef0123456789abcdef","extra":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var got struct{ Challenge string }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Challenge != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("body = %s, want the challenge echoed", rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

// Nudges within 10 s of the one that started a check are dropped but still
// answered 202; the next one after the interval starts another.
func TestReceiverDebouncesReleaseNudges(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	checks := 0
	h := newReceiver("v1alpha3", func() { checks++ }, func() time.Time { return now })
	release := `{"type":"release","channel":"v1alpha3","tag":"v0.2.3"}`

	for i := 0; i < 3; i++ {
		if rec := post(t, h, release); rec.Code != http.StatusAccepted {
			t.Fatalf("nudge %d: status = %d, want 202", i, rec.Code)
		}
		now = now.Add(time.Second)
	}
	if checks != 1 {
		t.Fatalf("checks = %d after three nudges in 3 s, want 1", checks)
	}
	now = now.Add(nudgeInterval)
	post(t, h, release)
	if checks != 2 {
		t.Fatalf("checks = %d after the interval, want 2", checks)
	}
}

func TestReceiverIgnoresOtherChannels(t *testing.T) {
	h := ReleaseHandler("v1alpha3", func() { t.Fatal("another channel's release must not start a check") })
	if rec := post(t, h, `{"type":"release","channel":"v1alpha2","tag":"v0.1.21"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
}

func TestReceiverRefusesBadBodies(t *testing.T) {
	h := ReleaseHandler("v1alpha3", func() { t.Fatal("a refused body must not start a check") })
	for name, body := range map[string]string{
		"oversized":       `{"type":"release","channel":"v1alpha3","pad":"` + strings.Repeat("x", maxBodyBytes) + `"}`,
		"unknown type":    `{"type":"ping"}`,
		"missing type":    `{"channel":"v1alpha3"}`,
		"not json":        `type=release`,
		"empty challenge": `{"type":"challenge"}`,
	} {
		if rec := post(t, h, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}
