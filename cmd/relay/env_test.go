package main

import (
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// clearRelayEnv empties every RELAY_* variable the process inherited.
func clearRelayEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "RELAY_") {
			t.Setenv(name, "")
		}
	}
}

func TestLogLevel(t *testing.T) {
	for value, want := range map[string]slog.Level{
		"": slog.LevelInfo, "info": slog.LevelInfo, "verbose": slog.LevelInfo,
		"debug": slog.LevelDebug, " DEBUG ": slog.LevelDebug,
		"warn": slog.LevelWarn, "Warning": slog.LevelWarn, "error": slog.LevelError,
	} {
		t.Setenv("RELAY_LOG_LEVEL", value)
		if got := logLevel(); got != want {
			t.Errorf("RELAY_LOG_LEVEL=%q → %v, want %v", value, got, want)
		}
	}
}

// A dotenv file sets what the environment leaves unset or empty and reports exactly those names; values are taken verbatim after the first "=".
func TestLoadDotEnv(t *testing.T) {
	path := writeEnvFile(t, t.TempDir(), "x.env", strings.Join([]string{
		"# comment",
		"",
		"  RELAY_T_PLAIN=plain  ",
		"RELAY_T_EQUALS=a=b=c",
		"RELAY_T_EMPTY=",
		"RELAY_T_QUOTED=\"kept as written\"",
		"RELAY_T_EXPORTED=from-file",
		"RELAY_T_BLANK_IN_ENV=from-file",
		"RELAY_T_TWICE=first",
		"RELAY_T_TWICE=second",
		"not an assignment",
		"  # indented comment",
	}, "\n"))
	names := []string{"RELAY_T_PLAIN", "RELAY_T_EQUALS", "RELAY_T_EMPTY", "RELAY_T_QUOTED", "RELAY_T_EXPORTED", "RELAY_T_BLANK_IN_ENV", "RELAY_T_TWICE"}
	for _, k := range names {
		t.Setenv(k, "")
	}
	t.Setenv("RELAY_T_EXPORTED", "exported")

	set, err := loadDotEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	wantSet := map[string]bool{"RELAY_T_PLAIN": true, "RELAY_T_EQUALS": true, "RELAY_T_EMPTY": true, "RELAY_T_QUOTED": true, "RELAY_T_BLANK_IN_ENV": true, "RELAY_T_TWICE": true}
	if !maps.Equal(set, wantSet) {
		t.Errorf("names set = %v, want %v", set, wantSet)
	}
	for k, want := range map[string]string{
		"RELAY_T_PLAIN": "plain", "RELAY_T_EQUALS": "a=b=c", "RELAY_T_EMPTY": "", "RELAY_T_QUOTED": `"kept as written"`,
		"RELAY_T_EXPORTED": "exported", "RELAY_T_BLANK_IN_ENV": "from-file", "RELAY_T_TWICE": "first",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
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
	if err := loadEnvFile([]string{"relay"}); err == nil || !strings.HasPrefix(err.Error(), "RELAY_ENV_FILE: open ") || dotEnvFile != "" {
		t.Fatalf("a missing RELAY_ENV_FILE: err = %v, recorded file %q", err, dotEnvFile)
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

func TestLoadEnvFileUnreadableDotEnvIsSkipped(t *testing.T) {
	withEnvFileState(t)
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := os.Open(".env"); !os.IsPermission(err) {
		t.Skipf("working directory still readable (running as root?): %v", err)
	}
	t.Setenv("RELAY_ENV_FILE", "")
	if err := loadEnvFile([]string{"relay"}); err != nil {
		t.Fatalf("unreadable working directory: %v", err)
	}
}
