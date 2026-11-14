package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
)

// mintServer stands in for the control listener over plain http: login sets a Secure session cookie for the right password, mint answers only when that cookie comes back, and every request is recorded.
type mintServer struct {
	mu        sync.Mutex
	reqs      []controlRequest
	mintReply string
	srv       *httptest.Server
}

func newMintServer(t *testing.T) *mintServer {
	t.Helper()
	m := &mintServer{mintReply: `{"token":"ey.jwt","expiresAt":"2030-01-01T00:00:00Z"}`}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.reqs = append(m.reqs, controlRequest{Method: r.Method, Path: r.URL.Path, ContentType: r.Header.Get("Content-Type"), Cookie: r.Header.Get("Cookie"), Body: string(body)})
		m.mu.Unlock()
		switch r.URL.Path {
		case "/api/auth/login":
			var creds struct{ Username, Password string }
			if json.Unmarshal(body, &creds) != nil || creds.Username != "alice" || creds.Password != "s3cret" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"bad credentials"}}`)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "relay_session", Value: "s-1", Path: "/", Secure: true, HttpOnly: true})
			_, _ = io.WriteString(w, `{}`)
		case "/api/auth/token":
			if c, err := r.Cookie("relay_session"); err != nil || c.Value != "s-1" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"no session"}}`)
				return
			}
			_, _ = io.WriteString(w, m.mintReply)
		case "/api/auth/logout":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// An admin token names no user, so mint logs in with a password, carries the session cookie onto the mint call even over plain http, prints the bare token and ends the session.
func TestRunToken_Mint(t *testing.T) {
	const session = "relay_session=s-1"
	login := controlRequest{Method: "POST", Path: "/api/auth/login", ContentType: "application/json", Body: `{"password":"s3cret","username":"alice"}`}
	logout := controlRequest{Method: "POST", Path: "/api/auth/logout", Cookie: session}
	mint := func(body string) controlRequest {
		return controlRequest{Method: "POST", Path: "/api/auth/token", ContentType: "application/json", Cookie: session, Body: body}
	}
	tests := []struct {
		name      string
		args      []string
		env       map[string]string
		mintReply string
		want      []controlRequest
		stdout    string
		code      int
		err       string
	}{
		{name: "project and ttl", args: []string{"mint", "--project", "ml-search", "--ttl", "30m", "--user", "alice"}, env: map[string]string{"RELAY_PASSWORD": "s3cret"},
			want: []controlRequest{login, mint(`{"project":"ml-search","ttl":"30m"}`), logout}, stdout: "ey.jwt\n"},
		{name: "user from the environment, server's default ttl", args: []string{"mint", "--project", "p1"}, env: map[string]string{"RELAY_PASSWORD": "s3cret", "RELAY_USER": "alice"},
			want: []controlRequest{login, mint(`{"project":"p1"}`), logout}, stdout: "ey.jwt\n"},
		{name: "reply without a token is printed as it came", args: []string{"mint", "--project", "p1", "--user", "alice"}, env: map[string]string{"RELAY_PASSWORD": "s3cret"},
			mintReply: `{"pending":true}`, want: []controlRequest{login, mint(`{"project":"p1"}`), logout}, stdout: `{"pending":true}`},
		{name: "wrong password", args: []string{"mint", "--project", "p1", "--user", "alice"}, env: map[string]string{"RELAY_PASSWORD": "nope"},
			want: []controlRequest{{Method: "POST", Path: "/api/auth/login", ContentType: "application/json", Body: `{"password":"nope","username":"alice"}`}},
			code: exitFailed, err: `token mint: login: server returned 401: {"error":{"message":"bad credentials"}}`},
		// With no terminal to prompt on, an unset password must stop the run, not fall through to a login with an empty one.
		{name: "no password and no terminal", args: []string{"mint", "--project", "p1", "--user", "alice"}, code: 1, err: "token mint: set $RELAY_PASSWORD (no terminal to prompt on)"},
		{name: "no user", args: []string{"mint", "--project", "p1"}, env: map[string]string{"RELAY_PASSWORD": "s3cret"}, code: 1, err: "token mint: --user (or $RELAY_USER) is required"},
		{name: "no project", args: []string{"mint", "--user", "alice"}, env: map[string]string{"RELAY_PASSWORD": "s3cret"}, code: 1, err: "token mint: --project is required"},
		{name: "another verb", args: []string{"revoke"}, code: 1, err: "usage: relay token mint --project <slug> [--ttl 1h]"},
		{name: "no verb", code: 1, err: "usage: relay token mint --project <slug> [--ttl 1h]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newMintServer(t)
			if tc.mintReply != "" {
				m.mintReply = tc.mintReply
			}
			for _, k := range []string{"RELAY_PASSWORD", "RELAY_USER", "RELAY_URL"} {
				t.Setenv(k, tc.env[k])
			}
			// A closed pipe is never a terminal.
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			w.Close()
			stdin := os.Stdin
			os.Stdin = r
			t.Cleanup(func() { os.Stdin = stdin; r.Close() })

			args := tc.args
			if len(args) > 0 {
				args = append(append([]string(nil), args...), "--url", m.srv.URL+"/")
			}
			out := captureStdout(t, func() { err = runToken(args) })
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			if exitCode(err) != tc.code || msg != tc.err || out != tc.stdout {
				t.Errorf("exit %d %q stdout %q; want %d %q %q", exitCode(err), msg, out, tc.code, tc.err, tc.stdout)
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if len(m.reqs) != len(tc.want) {
				t.Fatalf("requests = %+v\n    want %+v", m.reqs, tc.want)
			}
			for i := range tc.want {
				if m.reqs[i] != tc.want[i] {
					t.Errorf("request %d = %+v\n       want %+v", i, m.reqs[i], tc.want[i])
				}
			}
		})
	}
}
