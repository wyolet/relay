package config

import "testing"

func TestRedisPasswordFromEnv(t *testing.T) {
	t.Setenv("RELAY_REDIS_ADDR", "valkey:6379")
	t.Setenv("RELAY_REDIS_PASSWORD", "s3cret-valkey-pass")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RedisPassword != "s3cret-valkey-pass" {
		t.Fatalf("RedisPassword = %q", cfg.RedisPassword)
	}
}
