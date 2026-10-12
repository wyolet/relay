package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// reviewServer answers /api/access-review with answer, recording each request body it gets.
func reviewServer(t *testing.T, status int, answer string, got *[]map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/access-review" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body %s: %v", raw, err)
		}
		*got = append(*got, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const allowedAnswer = `{"allowed":true,"reason":"binding","action":"keys.get",
	"subject":{"kind":"user","name":"alice"},
	"resource":{"kind":"key","name":"k1","owner":{"kind":"project","name":"ml"}},
	"binding":{"name":"alice-dev","subject":{"kind":"user","name":"alice"},"role":{"kind":"role","name":"developer"},"scope":{"kind":"project","name":"ml"}}}`

const deniedAnswer = `{"allowed":false,"reason":"none","action":"keys.delete",
	"subject":{"kind":"serviceaccount","name":"ci","disabled":true},
	"resource":{"kind":"key","name":"k1"}}`

func TestCanIAllowedExitsZero(t *testing.T) {
	var got []map[string]any
	srv := reviewServer(t, http.StatusOK, allowedAnswer, &got)
	var err error
	out := captureStdout(t, func() {
		err = runAuth([]string{"can-i", "keys.get", "keys/k1", "--as", "alice", "--url", srv.URL})
	})
	if code := exitCode(t, err); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.HasPrefix(out, "yes: role binding alice-dev grants developer at project ml to user alice") {
		t.Fatalf("output = %q", out)
	}
	want := map[string]any{
		"action":   "keys.get",
		"resource": map[string]any{"kind": "keys", "name": "k1"},
		"subject":  map[string]any{"kind": "user", "name": "alice"},
	}
	if len(got) != 1 || !jsonEqual(got[0], want) {
		t.Fatalf("request = %v, want %v", got, want)
	}
}

func TestCanIDeniedExitsOne(t *testing.T) {
	var got []map[string]any
	srv := reviewServer(t, http.StatusOK, deniedAnswer, &got)
	var err error
	out := captureStdout(t, func() {
		err = runAuth([]string{"can-i", "--url", srv.URL, "delete", "keys", "--as", "sa:ci", "--owner", "project/ml"})
	})
	if code := exitCode(t, err); code != exitDenied {
		t.Fatalf("exit %d, want %d", code, exitDenied)
	}
	if !strings.Contains(out, "no: no rule or role binding grants keys.delete on key k1") || !strings.Contains(out, "is disabled") {
		t.Fatalf("output = %q", out)
	}
	want := map[string]any{
		"action":   "delete",
		"resource": map[string]any{"kind": "keys", "owner": map[string]any{"kind": "project", "name": "ml"}},
		"subject":  map[string]any{"kind": "serviceaccount", "name": "ci"},
	}
	if len(got) != 1 || !jsonEqual(got[0], want) {
		t.Fatalf("request = %v, want %v", got, want)
	}
}

func TestCanIFailuresExitTwo(t *testing.T) {
	var got []map[string]any
	srv := reviewServer(t, http.StatusForbidden, `{"error":{"message":"forbidden"}}`, &got)
	for _, args := range [][]string{
		{"can-i", "keys.get", "keys/k1", "--as", "bob", "--url", srv.URL},
		{"can-i", "keys.get"},
		{"whoami"},
		{},
	} {
		if code := exitCode(t, runAuth(args)); code != exitFailed {
			t.Fatalf("%v: exit %d, want %d", args, code, exitFailed)
		}
	}
}

func TestCanIPrintsJSON(t *testing.T) {
	var got []map[string]any
	srv := reviewServer(t, http.StatusOK, allowedAnswer, &got)
	out := captureStdout(t, func() {
		if err := runAuth([]string{"can-i", "get", "keys/k1", "-o", "json", "--url", srv.URL}); err != nil {
			t.Errorf("runAuth: %v", err)
		}
	})
	if out != allowedAnswer {
		t.Fatalf("output = %q, want the server's answer verbatim", out)
	}
	if _, asked := got[0]["subject"]; asked {
		t.Fatalf("no --as sent a subject: %v", got[0])
	}
}

func jsonEqual(a, b any) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return string(ra) == string(rb)
}
