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

// envInt64 reads an int64 env var, returning def if unset or invalid.
func envInt64(key string, def int64) int64 {
	if s := os.Getenv(key); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return v
		}
	}
	return def
}

// envBool reads a bool env var where "on"/"1" → true, "off"/"0"/"" → false.
// Returns (value, error). Non-empty non-matching values return an error.
func envBool(key string, validOn []string, validOff []string) (bool, bool, error) {
	s := os.Getenv(key)
	if s == "" {
		return false, true, nil
	}
	for _, v := range validOn {
		if s == v {
			return true, true, nil
		}
	}
	for _, v := range validOff {
		if s == v {
			return false, true, nil
		}
	}
	return false, false, nil
}
