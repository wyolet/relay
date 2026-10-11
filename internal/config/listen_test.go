package config

import "testing"

func TestListenHosts(t *testing.T) {
	for _, tc := range []struct {
		name, shared, control      string
		wantInference, wantControl string
		inferencePort, controlPort string
	}{
		{name: "unset binds every interface", wantInference: ":8080", wantControl: ":8081"},
		{name: "shared host covers both", shared: "127.0.0.1", wantInference: "127.0.0.1:8080", wantControl: "127.0.0.1:8081"},
		{name: "control override", control: "127.0.0.1", wantInference: ":8080", wantControl: "127.0.0.1:8081"},
		{name: "override wins over shared", shared: "10.0.0.5", control: "127.0.0.1", wantInference: "10.0.0.5:8080", wantControl: "127.0.0.1:8081"},
		{name: "ipv6 literal", shared: "::1", wantInference: "[::1]:8080", wantControl: "[::1]:8081"},
		{name: "bracketed ipv6", shared: "[::1]", wantInference: "[::1]:8080", wantControl: "[::1]:8081"},
		{name: "ports follow their variables", shared: "localhost", inferencePort: "9000", controlPort: "9001", wantInference: "localhost:9000", wantControl: "localhost:9001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RELAY_LISTEN_HOST", tc.shared)
			t.Setenv("RELAY_CONTROL_LISTEN_HOST", tc.control)
			t.Setenv("RELAY_PORT", tc.inferencePort)
			t.Setenv("RELAY_CONTROL_PORT", tc.controlPort)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.InferenceAddr(); got != tc.wantInference {
				t.Errorf("InferenceAddr = %q, want %q", got, tc.wantInference)
			}
			if got := cfg.ControlAddr(); got != tc.wantControl {
				t.Errorf("ControlAddr = %q, want %q", got, tc.wantControl)
			}
		})
	}
}

// A host that carries a port would bind somewhere the operator did not ask for, or fail at bind time; it fails at boot instead.
func TestListenHostRejectsAPort(t *testing.T) {
	for _, key := range []string{"RELAY_LISTEN_HOST", "RELAY_CONTROL_LISTEN_HOST"} {
		for _, v := range []string{"127.0.0.1:8080", "[::1]:8080", "unix:/tmp/relay.sock"} {
			t.Run(key+"="+v, func(t *testing.T) {
				t.Setenv("RELAY_LISTEN_HOST", "")
				t.Setenv("RELAY_CONTROL_LISTEN_HOST", "")
				t.Setenv(key, v)
				if _, err := Load(); err == nil {
					t.Fatalf("Load accepted %s=%q", key, v)
				}
			})
		}
	}
}
