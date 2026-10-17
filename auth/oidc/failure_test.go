package oidc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// rendered records each Failure Render receives and answers it with a fixed marker.
type rendered struct{ failures []*Failure }

func (rd *rendered) render(w http.ResponseWriter, _ *http.Request, f *Failure) {
	rd.failures = append(rd.failures, f)
	w.WriteHeader(299)
	_, _ = w.Write([]byte("rendered"))
}

// failurePaths drive each way a callback refuses a login, with the plain-text answer it gives without Render.
var failurePaths = []struct {
	name   string
	run    func(f *fixture) *httptest.ResponseRecorder
	status int
	body   string
	code   string
}{
	{"provider error", func(f *fixture) *httptest.ResponseRecorder { return f.callback("error=access_denied", nil) },
		http.StatusBadGateway, "login failed at identity provider: access_denied", CodeIDPError},
	{"no flow cookie", func(f *fixture) *httptest.ResponseRecorder { return f.callback("code=good&state=x", nil) },
		http.StatusBadRequest, "login flow expired — start again", CodeFlowExpired},
	{"state mismatch", func(f *fixture) *httptest.ResponseRecorder {
		_, c := f.start()
		return f.callback("code=good&state=forged", c)
	}, http.StatusBadRequest, "state mismatch", CodeStateMismatch},
	{"missing code", func(f *fixture) *httptest.ResponseRecorder {
		q, c := f.start()
		return f.callback("state="+q.Get("state"), c)
	}, http.StatusBadRequest, "missing code", CodeMissingCode},
	{"exchange failed", func(f *fixture) *httptest.ResponseRecorder { return f.login("bad") },
		http.StatusBadGateway, "code exchange failed", CodeExchangeFailed},
	{"invalid id_token", func(f *fixture) *httptest.ResponseRecorder {
		f.p.claims = map[string]any{"aud": "someone-else"}
		return f.login("good")
	}, http.StatusBadGateway, "identity provider returned an invalid id_token", CodeInvalidIdentity},
	{"no usable identity", func(f *fixture) *httptest.ResponseRecorder {
		f.p.claims = map[string]any{"sub": nil}
		return f.login("good")
	}, http.StatusBadGateway, "identity provider returned no usable identity", CodeInvalidIdentity},
	{"provision error", func(f *fixture) *httptest.ResponseRecorder {
		f.rp.Provision = func(context.Context, Claims) (string, error) { return "", errors.New("db down") }
		return f.login("good")
	}, http.StatusInternalServerError, "account provisioning failed", CodeLoginFailed},
	{"login error", func(f *fixture) *httptest.ResponseRecorder {
		f.rp.Login = func(http.ResponseWriter, *http.Request, string, Claims) error { return errors.New("kv down") }
		return f.login("good")
	}, http.StatusInternalServerError, "login failed", CodeLoginFailed},
	{"hook failure keeps its code", func(f *fixture) *httptest.ResponseRecorder {
		f.rp.Provision = func(context.Context, Claims) (string, error) {
			return "", &Failure{Status: http.StatusForbidden, Code: "closed", Message: "closed"}
		}
		return f.login("good")
	}, http.StatusForbidden, "closed", "closed"},
}

// Without Render every refusal is the plain-text answer it always was, and OnDenied sees its code.
func TestFailureCodesDefaultResponse(t *testing.T) {
	for _, tc := range failurePaths {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			w := tc.run(f)
			wantStatus(t, w, tc.status, tc.body)
			if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
				t.Errorf("content type %q", ct)
			}
			if len(f.rec.denied) != 1 || f.rec.denied[0].Code != tc.code {
				t.Fatalf("denied = %+v, want one with code %q", f.rec.denied, tc.code)
			}
		})
	}
}

func TestFailureCodesRender(t *testing.T) {
	for _, tc := range failurePaths {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			rd := &rendered{}
			f.rp.Render = rd.render
			wantStatus(t, tc.run(f), 299, "rendered")
			if len(rd.failures) != 1 || rd.failures[0].Code != tc.code || rd.failures[0].Status != tc.status || rd.failures[0].Message != tc.body {
				t.Fatalf("rendered = %+v, want %d %q %q", rd.failures, tc.status, tc.code, tc.body)
			}
			if len(f.rec.denied) != 1 || f.rec.denied[0].Code != tc.code {
				t.Fatalf("denied = %+v", f.rec.denied)
			}
		})
	}
}

// A provider that cannot be discovered at the callback is rendered, still without OnDenied.
func TestCallbackDiscoveryFailureRendered(t *testing.T) {
	f := newFixture(t)
	rd := &rendered{}
	f.rp.Render = rd.render
	q, c := f.start()
	f.rp.Config().Issuer = f.p.srv.URL + "/gone"
	wantStatus(t, f.callback("code=good&state="+q.Get("state"), c), 299, "rendered")
	if len(rd.failures) != 1 || rd.failures[0].Code != CodeIDPUnavailable || rd.failures[0].Status != http.StatusBadGateway || rd.failures[0].Err == nil {
		t.Fatalf("rendered = %+v", rd.failures)
	}
	if len(f.rec.denied) != 0 {
		t.Fatalf("discovery failure reported as denied: %+v", f.rec.denied)
	}
}
