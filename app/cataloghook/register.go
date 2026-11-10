package cataloghook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wyolet/relay/app/seed"
)

const (
	// renewInterval re-registers well inside the server's 24 h subscription TTL.
	renewInterval = 6 * time.Hour
	// retryDelay is the one extra attempt after a failed registration, e.g.
	// when the challenge reached no ready pod yet.
	retryDelay = time.Minute
	// registerTimeout covers the server's challenge round trip (5 s) with room.
	registerTimeout = 30 * time.Second
)

// Registrar subscribes HookURL to Channel's releases at the catalog server
// and keeps the subscription renewed. Failures are logged, never fatal.
type Registrar struct {
	// Endpoint is the catalog server's registration URL.
	Endpoint string
	// HookURL is this relay's public receiver URL.
	HookURL string
	Channel string
	Client  *http.Client
	Renew   time.Duration
	Retry   time.Duration
}

// NewRegistrar registers publicBase+ReleasePath with the catalog server at the
// origin of indexURL (empty = the default catalog index).
func NewRegistrar(indexURL, publicBase, channel string) (*Registrar, error) {
	if indexURL == "" {
		indexURL = seed.DefaultCatalogIndexURL
	}
	u, err := url.Parse(indexURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("catalog webhook: the catalog index URL %q has no origin to register at", indexURL)
	}
	return &Registrar{
		Endpoint: u.Scheme + "://" + u.Host + "/v1/webhooks",
		HookURL:  strings.TrimRight(publicBase, "/") + ReleasePath,
		Channel:  channel,
		Client:   &http.Client{Timeout: registerTimeout},
		Renew:    renewInterval,
		Retry:    retryDelay,
	}, nil
}

// Run registers now, again every Renew, and once more Retry after a failed
// attempt, until ctx ends. Start it once the receiver is being served: the
// server challenges HookURL before it answers.
func (r *Registrar) Run(ctx context.Context) {
	renew := time.NewTicker(r.Renew)
	defer renew.Stop()
	var retry <-chan time.Time
	attempt := func(isRetry bool) {
		err := r.register(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("catalog: webhook registration failed; polling continues", "url", r.HookURL, "err", err)
			if !isRetry {
				retry = time.After(r.Retry)
			}
			return
		}
		slog.Info("catalog: webhook registered", "url", r.HookURL, "channel", r.Channel)
	}
	attempt(false)
	for {
		select {
		case <-ctx.Done():
			return
		case <-renew.C:
			retry = nil
			attempt(false)
		case <-retry:
			retry = nil
			attempt(true)
		}
	}
}

func (r *Registrar) register(ctx context.Context) error {
	body, err := json.Marshal(struct {
		URL     string `json:"url"`
		Channel string `json:"channel"`
	}{r.HookURL, r.Channel})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: HTTP %d: %s", r.Endpoint, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
