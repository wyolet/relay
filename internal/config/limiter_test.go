package config

import (
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	for _, tc := range []struct {
		limiter, syncMS string
		want            string
		wantSync        time.Duration
		wantErr         bool
	}{
		{"", "", "hybrid", time.Second, false},
		{"hybrid", "250", "hybrid", 250 * time.Millisecond, false},
		{"Lua", "", "lua", time.Second, false},
		{"local", "", "", 0, true},
		{"hybrid", "0", "", 0, true},
	} {
		t.Run("RELAY_LIMITER="+tc.limiter+",RELAY_LIMITER_SYNC_MS="+tc.syncMS, func(t *testing.T) {
			t.Setenv("RELAY_LIMITER", tc.limiter)
			t.Setenv("RELAY_LIMITER_SYNC_MS", tc.syncMS)
			cfg, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load() err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && (cfg.Limiter != tc.want || cfg.LimiterSync != tc.wantSync) {
				t.Fatalf("Limiter, LimiterSync = %q, %v; want %q, %v", cfg.Limiter, cfg.LimiterSync, tc.want, tc.wantSync)
			}
		})
	}
}
