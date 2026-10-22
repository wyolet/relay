package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
)

// logLevel reads RELAY_LOG_LEVEL (debug|info|warn|error, default info). Parsed
// here rather than via config.Load because the logger is set up before config.
func logLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RELAY_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// dotEnvFile and dotEnvKeys record the dotenv file this invocation loaded and the variables it set, so a command can tell a value an operator exported from one it picked up from a file.
var (
	dotEnvFile string
	dotEnvKeys map[string]bool
)

// loadEnvFile loads RELAY_ENV_FILE when it is set, and otherwise .env from the working directory for the server only: an operator command must not take its target or credentials from whatever directory it runs in.
func loadEnvFile(args []string) error {
	path, explicit := os.Getenv("RELAY_ENV_FILE"), true
	if path == "" {
		if len(args) > 1 && subcommands[args[1]] != nil {
			return nil
		}
		path, explicit = ".env", false
	}
	keys, err := loadDotEnv(path)
	if err != nil {
		if !explicit && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("RELAY_ENV_FILE: %w", err)
	}
	dotEnvFile, dotEnvKeys = path, keys
	return nil
}

// loadDotEnv reads a .env file and sets any KEY=VALUE pair whose key is not
// already present in the environment, returning the keys it set. Comment
// lines and empty lines are skipped.
func loadDotEnv(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	set := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
			set[k] = true
		}
	}
	return set, sc.Err()
}
