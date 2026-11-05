package config

import (
	"testing"
	"time"
)

func TestConnQueue(t *testing.T) {
	for _, tc := range []struct {
		queue, timeout string
		wantQueue      int
		wantTimeout    time.Duration
		wantErr        bool
	}{
		{"", "", 1024, time.Second, false},
		{"0", "", 0, time.Second, false},
		{"4096", "250ms", 4096, 250 * time.Millisecond, false},
		{"-1", "", 0, 0, true},
		{"", "0s", 0, 0, true},
		{"", "-1s", 0, 0, true},
		{"", "1", 0, 0, true},
	} {
		t.Run("RELAY_CONN_QUEUE="+tc.queue+",RELAY_CONN_QUEUE_TIMEOUT="+tc.timeout, func(t *testing.T) {
			t.Setenv("RELAY_CONN_QUEUE", tc.queue)
			t.Setenv("RELAY_CONN_QUEUE_TIMEOUT", tc.timeout)
			cfg, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load() err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && (cfg.ConnQueue != tc.wantQueue || cfg.ConnQueueTimeout != tc.wantTimeout) {
				t.Fatalf("ConnQueue, ConnQueueTimeout = %d, %v; want %d, %v", cfg.ConnQueue, cfg.ConnQueueTimeout, tc.wantQueue, tc.wantTimeout)
			}
		})
	}
}
