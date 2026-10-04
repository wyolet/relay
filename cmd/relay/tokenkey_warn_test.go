package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/storage/gen"
	pkgsecret "github.com/wyolet/relay/pkg/secret"
)

// countingHandler tallies log records whose message contains a needle.
type countingHandler struct {
	slog.Handler
	mu     sync.Mutex
	needle string
	n      int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, h.needle) {
		h.mu.Lock()
		h.n++
		h.mu.Unlock()
	}
	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

// Boot runs loadTokenSigningKey, and the settings watcher's first delivery
// runs it again through applyAuthTokensSection. Without a master key both
// hit the same warning, which read as two independent problems in the boot
// log.
func TestTokensDisabledWarningLogsOnce(t *testing.T) {
	cfg := settings.AuthTokens{Enabled: true, DefaultTTL: time.Hour, MaxTTL: 24 * time.Hour}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	stores := &appcatalog.Stores{
		Settings: settings.NewStore(gen.New(stubDBTX{row: settingsRow{
			section: settings.AuthTokensSection, value: raw,
		}})),
		Secrets: pkgsecret.NewRegistry(),
	}

	h := &countingHandler{needle: "no RELAY_MASTER_KEY"}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Fresh sync.Once per run: the package-level one is process-wide.
	noMasterKeyWarn = sync.Once{}
	t.Cleanup(func() { noMasterKeyWarn = sync.Once{} })

	signer := &control.TokenSigner{}
	verifier := &inference.TokenVerifier{}
	ctx := context.Background()

	if err := loadTokenSigningKey(ctx, nil, stores, nil, signer, verifier); err != nil {
		t.Fatalf("boot path: %v", err)
	}
	if err := applyAuthTokensSection(ctx, nil, stores, nil, cfg, signer, verifier); err != nil {
		t.Fatalf("watcher path: %v", err)
	}

	h.mu.Lock()
	got := h.n
	h.mu.Unlock()
	if got != 1 {
		t.Fatalf("tokens-disabled warning logged %d times, want 1", got)
	}
}
