package config

import "testing"

func TestCatalogVersionDefault(t *testing.T) {
	stamped := DefaultCatalogVersion
	t.Cleanup(func() { DefaultCatalogVersion = stamped })

	for _, tc := range []struct {
		name, stamp, dir, version, want string
	}{
		{"unstamped build has no default", "", "", "", ""},
		{"stamped build defaults the version", "v1.2.3", "", "", "v1.2.3"},
		{"explicit version wins", "v1.2.3", "", "v9.9.9", "v9.9.9"},
		{"catalog dir keeps the default off", "v1.2.3", "/catalog", "", ""},
		{"catalog dir with an explicit version", "v1.2.3", "/catalog", "v9.9.9", "v9.9.9"},
		{"off disables the default", "v1.2.3", "", "off", ""},
		{"off on an unstamped build", "", "", "off", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			DefaultCatalogVersion = tc.stamp
			t.Setenv("RELAY_CATALOG_DIR", tc.dir)
			t.Setenv("RELAY_CATALOG_VERSION", tc.version)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.CatalogVersion != tc.want {
				t.Errorf("CatalogVersion = %q, want %q", cfg.CatalogVersion, tc.want)
			}
		})
	}
}
