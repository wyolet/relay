package config

import "testing"

func TestPGPoolSizeBounds(t *testing.T) {
	for _, key := range []string{"RELAY_PG_MAX_CONNS", "RELAY_PG_MIN_CONNS"} {
		for _, tc := range []struct {
			val     string
			wantErr bool
		}{
			{"20", false},
			{"2147483647", false},
			{"0", true},
			{"2147483648", true},
		} {
			t.Run(key+"="+tc.val, func(t *testing.T) {
				t.Setenv(key, tc.val)
				_, err := Load()
				if (err != nil) != tc.wantErr {
					t.Fatalf("Load() err = %v, wantErr %v", err, tc.wantErr)
				}
			})
		}
	}
}
