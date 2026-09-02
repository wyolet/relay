package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/storage/gen"
	pkgsecret "github.com/wyolet/relay/pkg/secret"
)

// The control listener's default URL has to be the control listener's
// default port, or every CLI subcommand goes to the wrong socket.
func TestDefaultControlURLMatchesTheControlPort(t *testing.T) {
	if !strings.HasSuffix(DefaultControlURL, ":8081") {
		t.Fatalf("DefaultControlURL = %q, want the control listener's default port", DefaultControlURL)
	}
}

// Tokens switched on after boot have no signing key yet; without the
// generate path in the watcher, minting stays 503 until the next restart.
func TestEnablingTokensLaterGeneratesASigningKey(t *testing.T) {
	cfg := settings.AuthTokens{Enabled: true, DefaultTTL: time.Hour, MaxTTL: 24 * time.Hour}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}
	db := stubDBTX{row: settingsRow{section: settings.AuthTokensSection, value: raw}}
	stores := &appcatalog.Stores{
		Settings: settings.NewStore(gen.New(db)),
		Secrets:  pkgsecret.NewRegistry(),
	}
	signer := &control.TokenSigner{}
	verifier := &inference.TokenVerifier{}

	// The generate path is the one that reports a missing master key; the
	// plain ref-install path says nothing. The log line is what proves the
	// watcher reached it.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if err := applyAuthTokensSection(context.Background(), stores, nil, cfg, signer, verifier); err != nil {
		t.Fatalf("applyAuthTokensSection: %v", err)
	}
	if !strings.Contains(buf.String(), "no RELAY_MASTER_KEY") {
		t.Fatalf("the watcher did not reach the key-generation path: %q", buf.String())
	}
	if pub := signer.PublicKey(); pub != nil {
		t.Errorf("a deployment with no master key must not get a signing key, got %x", pub)
	}

	// Disabling clears both sides without touching the stored ref.
	off := cfg
	off.Enabled = false
	if err := applyAuthTokensSection(context.Background(), stores, nil, off, signer, verifier); err != nil {
		t.Fatalf("applyAuthTokensSection (disabled): %v", err)
	}
	if signer.PublicKey() != nil {
		t.Error("disabling tokens left a signing key installed")
	}
}
