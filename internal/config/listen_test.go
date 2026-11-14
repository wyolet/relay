package config

import "testing"

func TestConfig_ListenAddrs(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cfg           Config
		wantInference string
		wantControl   string
	}{
		{"no host binds every interface", Config{Port: "8080", ControlPort: "8081"}, ":8080", ":8081"},
		{"IPv4 hosts", Config{ListenHost: "10.0.0.5", ControlListenHost: "127.0.0.1", Port: "9000", ControlPort: "9001"}, "10.0.0.5:9000", "127.0.0.1:9001"},
		{"IPv6 host is bracketed", Config{ListenHost: "::1", ControlListenHost: "::1", Port: "8080", ControlPort: "8081"}, "[::1]:8080", "[::1]:8081"},
		{"hostname", Config{ListenHost: "localhost", ControlListenHost: "localhost", Port: "8080", ControlPort: "8081"}, "localhost:8080", "localhost:8081"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.InferenceAddr(); got != tc.wantInference {
				t.Errorf("InferenceAddr = %q, want %q", got, tc.wantInference)
			}
			if got := tc.cfg.ControlAddr(); got != tc.wantControl {
				t.Errorf("ControlAddr = %q, want %q", got, tc.wantControl)
			}
		})
	}
}
