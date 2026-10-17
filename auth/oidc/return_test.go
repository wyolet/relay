package oidc

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const allowedReturn = "https://ui.test/keys?tab=a"

// returnFixture is a fixture whose ValidateReturn accepts URLs on https://ui.test only and records every value it is asked about.
func returnFixture(t *testing.T) (*fixture, *[]string) {
	t.Helper()
	f := newFixture(t)
	f.rp.Config().PostLoginURL = "https://post.test/"
	var asked []string
	f.rp.ValidateReturn = func(_ *http.Request, raw string) (string, bool) {
		asked = append(asked, raw)
		return raw, strings.HasPrefix(raw, "https://ui.test/")
	}
	return f, &asked
}

func startWithReturn(raw string) string { return "/start?rd=" + url.QueryEscape(raw) }

// ValidateReturn decides the target at Start and is asked again at the callback before the browser is sent there.
func TestValidateReturnAtStartAndCallback(t *testing.T) {
	f, asked := returnFixture(t)
	q, c := f.startAt(startWithReturn(allowedReturn))
	if got := flowOf(t, c).Return; got != allowedReturn {
		t.Fatalf("flow cookie return = %q", got)
	}
	w := f.callback("code=good&state="+q.Get("state"), c)
	if w.Code != http.StatusFound || w.Header().Get("Location") != allowedReturn {
		t.Fatalf("callback: %d %q", w.Code, w.Header().Get("Location"))
	}
	if len(*asked) != 2 || (*asked)[0] != allowedReturn || (*asked)[1] != allowedReturn {
		t.Fatalf("ValidateReturn asked %q, want the target at start and at the callback", *asked)
	}

	// Without the parameter nothing is asked and the browser goes to PostLoginURL.
	*asked = nil
	if w := f.login("good"); w.Header().Get("Location") != "https://post.test/" || len(*asked) != 0 {
		t.Fatalf("no return: location %q, asked %q", w.Header().Get("Location"), *asked)
	}
}

// The target ValidateReturn returns is the one stored, not the raw value.
func TestValidateReturnNormalises(t *testing.T) {
	f := newFixture(t)
	f.rp.ValidateReturn = func(_ *http.Request, raw string) (string, bool) { return strings.ToLower(raw), true }
	q, c := f.startAt(startWithReturn("https://UI.test/x"))
	if w := f.callback("code=good&state="+q.Get("state"), c); w.Header().Get("Location") != "https://ui.test/x" {
		t.Fatalf("location %q", w.Header().Get("Location"))
	}
}

// A return target ValidateReturn refuses is a 400 at Start: rendered when Render is set, plain text otherwise; no flow starts.
func TestValidateReturnRefusedAtStart(t *testing.T) {
	for _, withRender := range []bool{false, true} {
		f, _ := returnFixture(t)
		rd := &rendered{}
		if withRender {
			f.rp.Render = rd.render
		}
		w := httptest.NewRecorder()
		f.rp.Start(w, httptest.NewRequest("GET", startWithReturn("https://evil.test/"), nil))
		if len(w.Result().Cookies()) != 0 || w.Header().Get("Location") != "" {
			t.Fatalf("refused start set a flow: %v %q", w.Result().Cookies(), w.Header().Get("Location"))
		}
		if !withRender {
			wantStatus(t, w, http.StatusBadRequest, "return target not allowed")
			continue
		}
		wantStatus(t, w, 299, "rendered")
		if len(rd.failures) != 1 || rd.failures[0].Code != CodeInvalidReturnTo || rd.failures[0].Status != http.StatusBadRequest || rd.failures[0].Return != "" {
			t.Fatalf("rendered = %+v", rd.failures)
		}
		if len(f.rec.denied) != 0 {
			t.Fatalf("start refusal reported as denied: %+v", f.rec.denied)
		}
	}
}

// A target too long for the flow cookie is refused even when ValidateReturn accepts it.
func TestValidateReturnTooLong(t *testing.T) {
	f, _ := returnFixture(t)
	w := httptest.NewRecorder()
	f.rp.Start(w, httptest.NewRequest("GET", startWithReturn("https://ui.test/"+strings.Repeat("a", maxReturnLen)), nil))
	wantStatus(t, w, http.StatusBadRequest, "return target not allowed")
}

// A target planted in the unsigned flow cookie that ValidateReturn refuses is never followed, nor handed to Render.
func TestValidateReturnTamperedCookie(t *testing.T) {
	f, _ := returnFixture(t)
	q, c := f.startAt(startWithReturn(allowedReturn))
	fs := flowOf(t, c)
	fs.Return = "https://evil.test/"
	tampered := withFlow(c, fs)
	if w := f.callback("code=good&state="+q.Get("state"), tampered); w.Code != http.StatusFound || w.Header().Get("Location") != "https://post.test/" {
		t.Fatalf("tampered cookie: %d %q", w.Code, w.Header().Get("Location"))
	}

	rd := &rendered{}
	f.rp.Render = rd.render
	f.callback("code=good&state=forged", tampered)
	if len(rd.failures) != 1 || rd.failures[0].Return != "" {
		t.Fatalf("rendered = %+v, want no return target", rd.failures)
	}
}

// Render learns the re-validated return target of a refused login, the provider's own error included, which leaves the flow cookie in place.
func TestRenderGetsReturnTarget(t *testing.T) {
	f, _ := returnFixture(t)
	rd := &rendered{}
	f.rp.Render = rd.render

	_, c := f.startAt(startWithReturn(allowedReturn))
	f.callback("code=good&state=forged", c)

	_, c = f.startAt(startWithReturn(allowedReturn))
	w := f.callback("error=access_denied", c)
	if len(w.Result().Cookies()) != 0 {
		t.Fatalf("provider error touched the flow cookie: %v", w.Result().Cookies())
	}

	if len(rd.failures) != 2 {
		t.Fatalf("rendered %d failures", len(rd.failures))
	}
	for _, got := range rd.failures {
		if got.Return != allowedReturn {
			t.Errorf("%s: return %q, want %q", got.Code, got.Return, allowedReturn)
		}
	}
}

// Without ValidateReturn a refused login hands Render the same-origin path Start accepted.
func TestRenderGetsDefaultReturnPath(t *testing.T) {
	f := newFixture(t)
	rd := &rendered{}
	f.rp.Render = rd.render
	_, c := f.startAt("/start?rd=" + url.QueryEscape("/ui/keys"))
	f.callback("code=good&state=forged", c)
	if len(rd.failures) != 1 || rd.failures[0].Return != "/ui/keys" {
		t.Fatalf("rendered = %+v", rd.failures)
	}
}
