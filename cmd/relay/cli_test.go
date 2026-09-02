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

// token mint drives POST /auth/token, which lands with M3. The client is
// written against that route; unskip when it exists.
func TestTokenMintUsesPasswordLogin(t *testing.T) {
	t.Skip("POST /auth/token lands with the token milestone")
}
