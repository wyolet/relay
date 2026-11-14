package cataloghook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A challenge is echoed, a release of this relay's channel starts a check, and anything else is refused; only a release nudges.
func TestReceiver(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantStatus int
		wantBody   string
		wantNudge  string
	}{
		{"challenge", `{"type":"challenge","challenge":"0123456789abcdef0123456789abcdef","extra":1}`, http.StatusOK, `{"challenge":"0123456789abcdef0123456789abcdef"}` + "\n", ""},
		{"release of this channel", `{"type":"release","channel":"v1alpha3","tag":"v0.2.3"}`, http.StatusAccepted, "", "v0.2.3"},
		{"release of another channel", `{"type":"release","channel":"v1alpha2","tag":"v0.1.21"}`, http.StatusAccepted, "", ""},
		{"release of no channel", `{"type":"release","tag":"v0.1.21"}`, http.StatusAccepted, "", ""},
		{"body of exactly the limit", `{"type":"release","channel":"v1alpha3","tag":"` + strings.Repeat("x", maxBodyBytes-48) + `"}`, http.StatusAccepted, "", strings.Repeat("x", maxBodyBytes-48)},
		{"body one byte over the limit", `{"type":"release","channel":"v1alpha3","tag":"` + strings.Repeat("x", maxBodyBytes-47) + `"}`, http.StatusBadRequest, "body must be a JSON object of at most 4 KiB\n", ""},
		{"unknown type", `{"type":"ping"}`, http.StatusBadRequest, "type must be challenge or release\n", ""},
		{"missing type", `{"channel":"v1alpha3"}`, http.StatusBadRequest, "type must be challenge or release\n", ""},
		{"not json", `type=release`, http.StatusBadRequest, "body must be a JSON object\n", ""},
		{"empty body", ``, http.StatusBadRequest, "body must be a JSON object\n", ""},
		{"empty challenge", `{"type":"challenge"}`, http.StatusBadRequest, "challenge is required\n", ""},
	} {
		nudged := ""
		rec := post(ReleaseHandler("v1alpha3", func(tag string) { nudged += tag }), tc.body)
		if rec.Code != tc.wantStatus || rec.Body.String() != tc.wantBody || nudged != tc.wantNudge {
			t.Errorf("%s: status %d body %q nudge %q, want %d %q %q", tc.name, rec.Code, rec.Body, nudged, tc.wantStatus, tc.wantBody, tc.wantNudge)
		}
		if tc.name == "challenge" && rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("challenge Content-Type = %q, want application/json", rec.Header().Get("Content-Type"))
		}
	}
}

func post(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, ReleasePath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Nudges within 10 s of the one that started a check are dropped but still answered 202; the first one at the interval starts another. Each nudge carries its release's tag.
func TestReceiverDebouncesReleaseNudges(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	var tags []string
	h := newReceiver("v1alpha3", func(tag string) { tags = append(tags, tag) }, func() time.Time { return now })
	for _, step := range []struct {
		after time.Duration
		tag   string
	}{
		{0, "v0.2.3"}, {time.Second, "dropped-1"}, {nudgeInterval - time.Second - time.Nanosecond, "dropped-2"},
		{time.Nanosecond, "v0.2.4"}, {nudgeInterval - time.Nanosecond, "dropped-3"}, {time.Hour, "v0.2.5"},
	} {
		now = now.Add(step.after)
		if rec := post(h, `{"type":"release","channel":"v1alpha3","tag":"`+step.tag+`"}`); rec.Code != http.StatusAccepted {
			t.Errorf("nudge %s: status = %d, want 202", step.tag, rec.Code)
		}
	}
	if got := strings.Join(tags, ","); got != "v0.2.3,v0.2.4,v0.2.5" {
		t.Errorf("checks started for %s, want v0.2.3,v0.2.4,v0.2.5", got)
	}
}
