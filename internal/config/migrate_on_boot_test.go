package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestMigrateOnBootDefaultsToTrue(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{
		{"", true},
		{"on", true},
		{"true", true},
		{"off", false},
		{"0", false},
	} {
		t.Setenv("RELAY_MIGRATE_ON_BOOT", tc.env)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("RELAY_MIGRATE_ON_BOOT=%q: %v", tc.env, err)
		}
		if cfg.MigrateOnBoot != tc.want {
			t.Errorf("RELAY_MIGRATE_ON_BOOT=%q → %v, want %v", tc.env, cfg.MigrateOnBoot, tc.want)
		}
	}
	t.Setenv("RELAY_MIGRATE_ON_BOOT", "maybe")
	if _, err := Load(); err == nil {
		t.Error("an unparseable value was accepted")
	}
}

// The pair is contradictory: the deployment asked for multi-user but every
// authenticated caller is an admin.
func TestSingleAuthzWithMultiUserWarns(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	t.Setenv("RELAY_AUTHZ", AuthzSingle)
	t.Setenv("RELAY_MULTI_USER", "on")
	if _, err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(buf.String(), "RELAY_MULTI_USER") {
		t.Fatalf("no warning about the contradictory pair: %q", buf.String())
	}

	buf.Reset()
	t.Setenv("RELAY_MULTI_USER", "")
	if _, err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Contains(buf.String(), "RELAY_MULTI_USER") {
		t.Fatalf("warned without RELAY_MULTI_USER set: %q", buf.String())
	}
}
