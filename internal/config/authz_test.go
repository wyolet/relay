package config

import "testing"

// TestAuthzFromDeprecatedMultiUser pins the upgrade path: a deployment that
// only ever set RELAY_MULTI_USER must not fall back to the single-user
// authorizer, where every authenticated user is an admin.
func TestAuthzFromDeprecatedMultiUser(t *testing.T) {
	for _, tc := range []struct {
		name, authz, multiUser, want string
	}{
		{"unset", "", "", AuthzSingle},
		{"multi user on", "", "on", AuthzRBAC},
		{"multi user true", "", "TRUE", AuthzRBAC},
		{"multi user off", "", "off", AuthzSingle},
		{"authz wins over multi user", AuthzSingle, "on", AuthzSingle},
		{"authz rbac", AuthzRBAC, "", AuthzRBAC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RELAY_AUTHZ", tc.authz)
			t.Setenv("RELAY_MULTI_USER", tc.multiUser)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Authz != tc.want {
				t.Fatalf("Authz = %q, want %q", cfg.Authz, tc.want)
			}
		})
	}
}
