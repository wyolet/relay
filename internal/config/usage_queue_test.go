package config

import "testing"

func TestUsageQueueSize(t *testing.T) {
	for _, tc := range []struct {
		val     string
		want    int
		wantErr bool
	}{
		{"", 0, false},
		{"32768", 32768, false},
		{"0", 0, true},
		{"-5", 0, true},
	} {
		t.Run("RELAY_USAGE_QUEUE_SIZE="+tc.val, func(t *testing.T) {
			t.Setenv("RELAY_USAGE_QUEUE_SIZE", tc.val)
			cfg, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load() err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && cfg.UsageQueueSize != tc.want {
				t.Fatalf("UsageQueueSize = %d, want %d", cfg.UsageQueueSize, tc.want)
			}
		})
	}
}
