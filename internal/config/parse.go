package config

import (
	"os"
	"strconv"
)

// envInt reads an integer env var, returning def if unset or invalid.
func envInt(key string, def int) int {
	if s := os.Getenv(key); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			return v
		}
	}
	return def
}

// envPositiveInt reads a positive integer env var, returning def if unset or invalid.
// Values below 1 are considered configuration errors.
func envPositiveInt(key string, def int) (int, error) {
	if s := os.Getenv(key); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			if v < 1 {
				return 0, strconv.ErrSyntax
			}
			return v, nil
		}
	}
	return def, nil
}

// envInt64 reads an int64 env var, returning def if unset or invalid.
func envInt64(key string, def int64) int64 {
	if s := os.Getenv(key); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return v
		}
	}
	return def
}
