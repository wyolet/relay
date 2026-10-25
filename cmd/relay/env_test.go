package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wyolet/relay/app/apply"
)

// withEnvFileState restores the recorded dotenv state after a test that loads one.
func withEnvFileState(t *testing.T) {
	t.Helper()
	file, keys := dotEnvFile, dotEnvKeys
	t.Cleanup(func() { dotEnvFile, dotEnvKeys = file, keys })
	dotEnvFile, dotEnvKeys = "", nil
}

func writeEnvFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadEnvFileDotEnvOnlyForTheServer(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"relay"}, "from-dotenv"},
		{[]string{"relay", "serve"}, "from-dotenv"},
		{[]string{"relay", "apply", "-f", "x"}, ""},
		{[]string{"relay", "migrate", "up"}, ""},
		{[]string{"relay", "secret", "status"}, ""},
	} {
		t.Run(strings.Join(tc.args[1:], " "), func(t *testing.T) {
			withEnvFileState(t)
			dir := t.TempDir()
			writeEnvFile(t, dir, ".env", "RELAY_TEST_DOTENV_VAR=from-dotenv\n")
			t.Chdir(dir)
			t.Setenv("RELAY_ENV_FILE", "")
			t.Setenv("RELAY_TEST_DOTENV_VAR", "")

			if err := loadEnvFile(tc.args); err != nil {
				t.Fatal(err)
			}
			if got := os.Getenv("RELAY_TEST_DOTENV_VAR"); got != tc.want {
				t.Fatalf("RELAY_TEST_DOTENV_VAR = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoadEnvFileExplicitFileForAnyCommand(t *testing.T) {
	withEnvFileState(t)
	dir := t.TempDir()
	path := writeEnvFile(t, dir, "prod.env", "# comment\nRELAY_TEST_DOTENV_VAR=from-file\nRELAY_TEST_DOTENV_KEEP=from-file\n")
	t.Setenv("RELAY_ENV_FILE", path)
	t.Setenv("RELAY_TEST_DOTENV_VAR", "")
	t.Setenv("RELAY_TEST_DOTENV_KEEP", "exported")

	if err := loadEnvFile([]string{"relay", "apply"}); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("RELAY_TEST_DOTENV_VAR"); got != "from-file" {
		t.Fatalf("RELAY_TEST_DOTENV_VAR = %q, want from-file", got)
	}
	if got := os.Getenv("RELAY_TEST_DOTENV_KEEP"); got != "exported" {
		t.Fatalf("an exported variable was overridden: %q", got)
	}
	if !dotEnvKeys["RELAY_TEST_DOTENV_VAR"] || dotEnvKeys["RELAY_TEST_DOTENV_KEEP"] {
		t.Fatalf("dotEnvKeys = %v, want only the variable the file set", dotEnvKeys)
	}
	if dotEnvFile != path {
		t.Fatalf("dotEnvFile = %q, want %q", dotEnvFile, path)
	}
}

func TestLoadEnvFileMissingExplicitFileFails(t *testing.T) {
	withEnvFileState(t)
	t.Setenv("RELAY_ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	if err := loadEnvFile([]string{"relay"}); err == nil {
		t.Fatal("a missing RELAY_ENV_FILE loaded without error")
	}
}

func TestLoadEnvFileMissingDotEnvIsFine(t *testing.T) {
	withEnvFileState(t)
	t.Chdir(t.TempDir())
	t.Setenv("RELAY_ENV_FILE", "")
	if err := loadEnvFile([]string{"relay"}); err != nil {
		t.Fatalf("no .env: %v", err)
	}
}

func TestApplyRefusesDotEnvTokenWithoutURL(t *testing.T) {
	withEnvFileState(t)
	dotEnvFile, dotEnvKeys = "prod.env", map[string]bool{"RELAY_ADMIN_TOKEN": true}
	t.Setenv("RELAY_ADMIN_TOKEN", "from-file")

	var requests atomic.Int32
	plan := planServer(t, http.StatusOK, []apply.Entry{{Kind: "Team", Name: "platform", Action: apply.ActionCreate}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		plan.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("RELAY_URL", srv.URL)

	err := runApply([]string{"-f", manifestDir(t), "--dry-run"})
	if err == nil || !strings.Contains(err.Error(), "prod.env") {
		t.Fatalf("apply with a dotenv token and no --url: err = %v, want a refusal naming the file", err)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("apply reached the server %d time(s) before refusing", n)
	}

	captureStdout(t, func() {
		err = runApply([]string{"-f", manifestDir(t), "--dry-run", "--url", srv.URL})
	})
	if err != nil {
		t.Fatalf("apply with an explicit --url: %v", err)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("apply with --url made %d request(s), want 1", n)
	}
}
