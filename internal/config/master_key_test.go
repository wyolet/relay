package config

import (
	"bytes"
	"strings"
	"testing"
)

func TestMasterKeyExtra(t *testing.T) {
	a, b, c := strings.Repeat("A", 43)+"=", strings.Repeat("B", 43)+"=", strings.Repeat("C", 43)+"="
	t.Setenv("RELAY_MASTER_KEY", a)
	t.Setenv("RELAY_MASTER_KEY_EXTRA", b+", "+c)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MasterKey) != 32 || len(cfg.MasterKeyExtra) != 2 {
		t.Fatalf("master key %d bytes, extra %d keys", len(cfg.MasterKey), len(cfg.MasterKeyExtra))
	}
	if bytes.Equal(cfg.MasterKeyExtra[0], cfg.MasterKeyExtra[1]) || bytes.Equal(cfg.MasterKey, cfg.MasterKeyExtra[0]) {
		t.Fatal("extra keys parsed to the wrong bytes")
	}

	t.Setenv("RELAY_MASTER_KEY_EXTRA", b+",short")
	if _, err := Load(); err == nil {
		t.Error("an invalid extra key was accepted")
	}

	t.Setenv("RELAY_MASTER_KEY", "")
	t.Setenv("RELAY_MASTER_KEY_EXTRA", b)
	if _, err := Load(); err == nil {
		t.Error("RELAY_MASTER_KEY_EXTRA without RELAY_MASTER_KEY was accepted")
	}
}
