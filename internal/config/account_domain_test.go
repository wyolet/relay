package config

import "testing"

func TestAccountDomain(t *testing.T) {
	for _, tc := range []struct {
		env, want string
		wantErr   bool
	}{
		{"", "", false},
		{"Accounts.Example.com.", "accounts.example.com", false},
		{".accounts.example.com", "accounts.example.com", false},
		{"https://accounts.example.com", "", true},
		{"accounts.example.com:8080", "", true},
	} {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("RELAY_ACCOUNT_DOMAIN", tc.env)
			cfg, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load() err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && cfg.AccountDomain != tc.want {
				t.Fatalf("AccountDomain = %q, want %q", cfg.AccountDomain, tc.want)
			}
		})
	}
}
