package config

import (
	"testing"
	"time"
)

func TestInflightSchedP99(t *testing.T) {
	for _, tc := range []struct {
		val     string
		want    time.Duration
		wantErr bool
	}{
		{"", 5 * time.Millisecond, false},
		{"2ms", 2 * time.Millisecond, false},
		{"0", 0, false},
		{"-1ms", 0, true},
		{"5", 0, true},
	} {
		t.Run("RELAY_INFLIGHT_SCHED_P99="+tc.val, func(t *testing.T) {
			t.Setenv("RELAY_INFLIGHT_SCHED_P99", tc.val)
			cfg, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load() err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && cfg.InflightSchedP99 != tc.want {
				t.Fatalf("InflightSchedP99 = %v, want %v", cfg.InflightSchedP99, tc.want)
			}
		})
	}
}
