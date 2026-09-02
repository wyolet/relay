package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/key"
)

const teamManifest = `apiVersion: relay.wyolet.dev/v1alpha2
kind: Team
metadata:
  name: platform
  displayName: Platform
spec:
  enabled: true
`

func manifestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(teamManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// planServer answers /api/apply with the given plan, or the given status.
func planServer(t *testing.T, status int, plan []apply.Entry) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/apply" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "kind: Team") {
			t.Errorf("server received an unexpected bundle: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"plan": plan, "applied": true})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig
	out, _ := io.ReadAll(r)
	return string(out)
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	t.Fatalf("unexpected error: %v", err)
	return -1
}

func TestApplyDryRunExitsClean(t *testing.T) {
	srv := planServer(t, http.StatusOK, []apply.Entry{
		{Kind: "Team", Name: "platform", Action: apply.ActionCreate},
	})
	var err error
	out := captureStdout(t, func() {
		err = runApply([]string{"-f", manifestDir(t), "--dry-run", "--url", srv.URL})
	})
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("exit %d, want 0", got)
	}
	for _, want := range []string{"Team", "platform", "create"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan output missing %q:\n%s", want, out)
		}
	}
}

func TestApplyDriftExitsOne(t *testing.T) {
	srv := planServer(t, http.StatusOK, []apply.Entry{
		{Kind: "Team", Name: "platform", Action: apply.ActionSkipDirty},
	})
	var err error
	captureStdout(t, func() {
		err = runApply([]string{"-f", manifestDir(t), "--url", srv.URL})
	})
	if got := exitCode(t, err); got != exitDrift {
		t.Fatalf("exit %d, want %d", got, exitDrift)
	}

	// --force is the operator saying "overwrite it"; drift stops being an error.
	captureStdout(t, func() {
		err = runApply([]string{"-f", manifestDir(t), "--force", "--url", srv.URL})
	})
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("--force exit %d, want 0", got)
	}
}

func TestApplyServerErrorExitsTwo(t *testing.T) {
	srv := planServer(t, http.StatusForbidden, nil)
	err := runApply([]string{"-f", manifestDir(t), "--url", srv.URL})
	if got := exitCode(t, err); got != exitFailed {
		t.Fatalf("exit %d, want %d", got, exitFailed)
	}
}

func TestKeygenPrintsAHashOfItsOwnPlaintext(t *testing.T) {
	out := captureStdout(t, func() {
		if err := runKeygen(nil); err != nil {
			t.Fatal(err)
		}
	})
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("unparsable keygen line %q", line)
		}
		fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	sum := sha256.Sum256([]byte(fields["plaintext"]))
	if hex.EncodeToString(sum[:]) != fields["keyHash"] {
		t.Fatalf("keyHash does not hash the printed plaintext: %v", fields)
	}
	if !strings.HasPrefix(fields["plaintext"], fields["prefix"]) || fields["prefix"] == "" {
		t.Fatalf("prefix is not the head of the plaintext: %v", fields)
	}
	if !strings.HasPrefix(fields["plaintext"], key.TokenPrefix) {
		t.Fatalf("plaintext %q lacks the key prefix", fields["plaintext"])
	}
}

// TestTokenMintUsesPasswordLogin pins the flow the mint route requires: an
// admin token names no user, so the CLI logs in and carries the session
// cookie onto POST /api/auth/token.
func TestTokenMintUsesPasswordLogin(t *testing.T) {
	const sessionCookie = "relay_session"
	var mintAuthed bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			var body struct{ Username, Password string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Username != "alice" || body.Password != "s3cret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "sess-1", Path: "/"})
			_ = json.NewEncoder(w).Encode(map[string]string{"user_id": "u-1", "username": "alice"})
		case "/api/auth/token":
			if c, err := r.Cookie(sessionCookie); err == nil && c.Value == "sess-1" {
				mintAuthed = true
			}
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["project"] != "ml-search" || body["ttl"] != "30m" {
				t.Errorf("mint body = %v, want the project and ttl the flags named", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token": "eyJhbGciOiJFZERTQSJ9.payload.sig", "jti": "j-1",
				"expiresAt": "2026-09-02T10:00:00Z", "project": "ml-search",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	out := captureStdout(t, func() {
		t.Setenv("RELAY_PASSWORD", "s3cret")
		if err := runToken([]string{"mint", "--project", "ml-search", "--ttl", "30m",
			"--user", "alice", "--url", srv.URL}); err != nil {
			t.Fatalf("token mint: %v", err)
		}
	})
	if !mintAuthed {
		t.Error("the mint request carried no session cookie")
	}
	if got := strings.TrimSpace(out); got != "eyJhbGciOiJFZERTQSJ9.payload.sig" {
		t.Errorf("stdout = %q, want the bare token", got)
	}
}

// TestTokenMintWithNoPasswordAndNoTerminalFails pins readPassword's
// non-interactive guard: without $RELAY_PASSWORD and with no terminal to
// prompt on, it must error rather than fall through to a login attempt with
// an empty password.
func TestTokenMintWithNoPasswordAndNoTerminalFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request should reach the server: %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("RELAY_PASSWORD", "")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	w.Close() // a closed pipe read-end is never a terminal
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })

	err = runToken([]string{"mint", "--project", "ml-search", "--user", "alice", "--url", srv.URL})
	if err == nil {
		t.Fatal("runToken = nil, want an error for no password and no terminal")
	}
	if !strings.Contains(err.Error(), "RELAY_PASSWORD") {
		t.Errorf("err = %v, want it to name RELAY_PASSWORD as the fix", err)
	}
}
