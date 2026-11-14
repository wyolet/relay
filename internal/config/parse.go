package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// envInt reads an integer env var, returning def if unset. A value that is
// not an integer is an error naming the variable.
func envInt(key string, def int) (int, error) {
	s := os.Getenv(key)
	if s == "" {
		return def, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, notInteger(key, s)
	}
	return v, nil
}

// envIntMin is envInt with a set value held to at least floor; def is not checked.
func envIntMin(key string, def, floor int) (int, error) {
	v, err := envInt(key, def)
	if err == nil && os.Getenv(key) != "" && v < floor {
		return 0, fmt.Errorf("%s must be >= %d", key, floor)
	}
	return v, err
}

// envPositiveInt is envIntMin with a minimum of 1.
func envPositiveInt(key string, def int) (int, error) { return envIntMin(key, def, 1) }

// envDuration reads a Go duration env var (e.g. "1s", "500ms"), returning def if unset.
func envDuration(key string, def time.Duration) (time.Duration, error) {
	if s := os.Getenv(key); s != "" {
		return time.ParseDuration(s)
	}
	return def, nil
}

// envInt64 reads an int64 env var, returning def if unset. A value that is
// not an integer is an error naming the variable.
func envInt64(key string, def int64) (int64, error) {
	s := os.Getenv(key)
	if s == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, notInteger(key, s)
	}
	return v, nil
}

func notInteger(key, value string) error {
	return fmt.Errorf("%s must be an integer, got %q", key, value)
}
