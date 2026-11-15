package v1

import (
	"testing"
	"time"
)

func TestCacheConfig_TTLDuration(t *testing.T) {
	tests := []struct {
		name string
		cfg  *CacheConfig
		want time.Duration
		ok   bool
	}{
		{"nil config", nil, 0, false},
		{"unset", &CacheConfig{}, 0, false},
		{"minutes", &CacheConfig{TTL: "5m"}, 5 * time.Minute, true},
		{"compound", &CacheConfig{TTL: "1h30m"}, 90 * time.Minute, true},
		{"zero", &CacheConfig{TTL: "0s"}, 0, false},
		{"negative", &CacheConfig{TTL: "-1h"}, 0, false},
		{"bare number", &CacheConfig{TTL: "300"}, 0, false},
		{"unknown unit", &CacheConfig{TTL: "1d"}, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := tc.cfg.TTLDuration(); got != tc.want || ok != tc.ok {
				t.Errorf("TTLDuration = %v, %v; want %v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}
