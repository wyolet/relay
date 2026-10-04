package main

import (
	"bufio"
	"log/slog"
	"os"
	"strings"
)

// loadDotEnv reads a .env file and sets any KEY=VALUE pair whose key is not
// already present in the environment. Comment lines and empty lines are skipped.
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

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

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
		}
	}
}
