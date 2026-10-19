package config

import "testing"

func TestPprofAddr(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{env: "", want: ""},
		{env: "127.0.0.1:6060", want: "127.0.0.1:6060"},
		{env: " :6060 ", want: ":6060"},
		{env: "[::1]:6060", want: "[::1]:6060"},
	} {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("RELAY_PPROF_ADDR", tc.env)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.PprofAddr != tc.want {
				t.Fatalf("PprofAddr = %q, want %q", cfg.PprofAddr, tc.want)
			}
		})
	}
}

func TestPprofAddrRejectsAMissingPort(t *testing.T) {
	for _, v := range []string{"127.0.0.1", "6060", "localhost"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("RELAY_PPROF_ADDR", v)
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted RELAY_PPROF_ADDR=%q", v)
			}
		})
	}
}
