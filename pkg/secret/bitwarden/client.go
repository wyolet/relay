package bitwarden

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type client struct {
	baseURL      string
	email        string
	password     string
	clientID     string
	clientSecret string
	httpClient   *http.Client
	deviceID     string

	mu           sync.RWMutex
	accessToken  string
	refreshToken string
	tokenExpiry  time.Time
	symKey       symmetricKey
}

func newClient(cfg Config) *client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.InsecureSkipVerify {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		}
		transport.TLSClientConfig.InsecureSkipVerify = true
	}

	deviceID := cfg.DeviceID
	if deviceID == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err == nil {
			deviceID = hex.EncodeToString(b[:])
		}
	}

	return &client{
		baseURL:      strings.TrimSuffix(cfg.BaseURL, "/"),
		email:        cfg.Email,
		password:     cfg.MasterPassword,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
		deviceID: deviceID,
	}
}

func (c *client) postForm(ctx context.Context, endpoint string, data url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.httpClient.Do(req)
}

func (c *client) doGET(ctx context.Context, endpoint, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return c.httpClient.Do(req)
}
