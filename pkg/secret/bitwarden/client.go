package bitwarden

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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

func (c *client) authenticate(ctx context.Context) error {
	prelogin, err := c.prelogin(ctx)
	if err != nil {
		return fmt.Errorf("bitwarden: prelogin: %w", err)
	}

	masterKey, err := makeMasterKey(c.password, c.email, prelogin.KDF, prelogin.KDFIterations, prelogin.KDFMemory, prelogin.KDFParallelism)
	if err != nil {
		return fmt.Errorf("bitwarden: derive master key: %w", err)
	}

	if c.clientID == "" || c.clientSecret == "" {
		return fmt.Errorf("bitwarden: client_id and client_secret are required")
	}

	tokenResp, err := c.loginWithAPIKey(ctx)
	if err != nil {
		return fmt.Errorf("bitwarden: login: %w", err)
	}

	encryptedKey := tokenResp.Key
	if encryptedKey == "" {
		c.mu.Lock()
		c.accessToken = tokenResp.AccessToken
		c.refreshToken = tokenResp.RefreshToken
		c.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
		c.mu.Unlock()

		encryptedKey, err = c.fetchProfileKey(ctx)
		if err != nil {
			return fmt.Errorf("bitwarden: fetch profile key: %w", err)
		}
	} else {
		c.mu.Lock()
		c.accessToken = tokenResp.AccessToken
		c.refreshToken = tokenResp.RefreshToken
		c.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
		c.mu.Unlock()
	}

	symKey, err := decryptSymmetricKey(encryptedKey, masterKey)
	if err != nil {
		return fmt.Errorf("bitwarden: decrypt symmetric key: %w", err)
	}

	c.mu.Lock()
	c.symKey = symKey
	c.mu.Unlock()

	return nil
}

func (c *client) refreshAccessToken(ctx context.Context) error {
	c.mu.RLock()
	rt := c.refreshToken
	c.mu.RUnlock()

	if rt == "" {
		return fmt.Errorf("bitwarden: no refresh token available")
	}

	data := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rt},
		"client_id":     {"web"},
	}

	resp, err := c.postForm(ctx, c.baseURL+"/identity/connect/token", data)
	if err != nil {
		return fmt.Errorf("bitwarden: refresh request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("bitwarden: refresh failed (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var tokenResp tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("bitwarden: decode refresh response: %w", err)
	}

	c.mu.Lock()
	c.accessToken = tokenResp.AccessToken
	if tokenResp.RefreshToken != "" {
		c.refreshToken = tokenResp.RefreshToken
	}
	c.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	c.mu.Unlock()

	return nil
}

func (c *client) ensureValidToken(ctx context.Context) error {
	c.mu.RLock()
	expiry := c.tokenExpiry
	c.mu.RUnlock()

	if time.Now().After(expiry.Add(-60 * time.Second)) {
		if err := c.refreshAccessToken(ctx); err != nil {
			return c.authenticate(ctx)
		}
	}
	return nil
}

func (c *client) sync(ctx context.Context) ([]decryptedItem, error) {
	if err := c.ensureValidToken(ctx); err != nil {
		return nil, fmt.Errorf("bitwarden: ensure valid token: %w", err)
	}

	syncResp, err := c.fetchSync(ctx)
	if err != nil {
		return nil, err
	}

	c.mu.RLock()
	userKey := c.symKey
	c.mu.RUnlock()

	orgKeys := make(map[string]symmetricKey)
	if len(syncResp.Profile.Organizations) > 0 && syncResp.Profile.PrivateKey != "" {
		privateKey, err := decryptPrivateKey(syncResp.Profile.PrivateKey, userKey)
		if err != nil {
			return nil, fmt.Errorf("bitwarden: decrypt RSA private key: %w", err)
		}
		for _, org := range syncResp.Profile.Organizations {
			orgKey, err := decryptOrgKey(org.Key, privateKey)
			if err != nil {
				return nil, fmt.Errorf("bitwarden: decrypt org key for %s: %w", org.ID, err)
			}
			orgKeys[org.ID] = orgKey
		}
	}

	items := make([]decryptedItem, 0, len(syncResp.Ciphers))
	for _, cipher := range syncResp.Ciphers {
		decryptKey := userKey
		if cipher.OrganizationID != nil && *cipher.OrganizationID != "" {
			orgKey, ok := orgKeys[*cipher.OrganizationID]
			if !ok {
				return nil, fmt.Errorf("bitwarden: no org key for cipher %s (org %s)", cipher.ID, *cipher.OrganizationID)
			}
			decryptKey = orgKey
		}

		item, err := decryptCipher(cipher, decryptKey)
		if err != nil {
			return nil, fmt.Errorf("bitwarden: decrypt cipher %s: %w", cipher.ID, err)
		}
		items = append(items, item)
	}

	return items, nil
}

func (c *client) fetchSync(ctx context.Context) (*syncResponse, error) {
	c.mu.RLock()
	token := c.accessToken
	c.mu.RUnlock()

	resp, err := c.doGET(ctx, c.baseURL+"/api/sync", token)
	if err != nil {
		return nil, fmt.Errorf("bitwarden: sync request: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if err := c.refreshAccessToken(ctx); err != nil {
			return nil, fmt.Errorf("bitwarden: sync auth failed: %w", err)
		}
		c.mu.RLock()
		token = c.accessToken
		c.mu.RUnlock()

		resp, err = c.doGET(ctx, c.baseURL+"/api/sync", token)
		if err != nil {
			return nil, fmt.Errorf("bitwarden: sync retry: %w", err)
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("bitwarden: sync failed (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var syncResp syncResponse
	if err := json.NewDecoder(resp.Body).Decode(&syncResp); err != nil {
		return nil, fmt.Errorf("bitwarden: decode sync response: %w", err)
	}

	return &syncResp, nil
}

func decryptCipher(c syncCipher, key symmetricKey) (decryptedItem, error) {
	item := decryptedItem{
		id:     c.ID,
		fields: make(map[string]string),
	}

	var err error
	item.name, err = decryptStr(c.Name, key)
	if err != nil {
		return item, fmt.Errorf("decrypt name: %w", err)
	}

	if c.Notes != nil && *c.Notes != "" {
		item.notes, err = decryptStr(*c.Notes, key)
		if err != nil {
			return item, fmt.Errorf("decrypt notes: %w", err)
		}
	}

	if c.Login != nil {
		if c.Login.Username != nil && *c.Login.Username != "" {
			item.username, err = decryptStr(*c.Login.Username, key)
			if err != nil {
				return item, fmt.Errorf("decrypt username: %w", err)
			}
		}
		if c.Login.Password != nil && *c.Login.Password != "" {
			item.password, err = decryptStr(*c.Login.Password, key)
			if err != nil {
				return item, fmt.Errorf("decrypt password: %w", err)
			}
		}
		if c.Login.URI != nil && *c.Login.URI != "" {
			item.uri, err = decryptStr(*c.Login.URI, key)
			if err != nil {
				return item, fmt.Errorf("decrypt uri: %w", err)
			}
		}
		if item.uri == "" && len(c.Login.URIs) > 0 && c.Login.URIs[0].URI != nil && *c.Login.URIs[0].URI != "" {
			item.uri, err = decryptStr(*c.Login.URIs[0].URI, key)
			if err != nil {
				return item, fmt.Errorf("decrypt uri: %w", err)
			}
		}
	}

	for _, f := range c.Fields {
		var name, value string
		if f.Name != nil && *f.Name != "" {
			name, err = decryptStr(*f.Name, key)
			if err != nil {
				return item, fmt.Errorf("decrypt field name: %w", err)
			}
		}
		if f.Value != nil && *f.Value != "" {
			value, err = decryptStr(*f.Value, key)
			if err != nil {
				return item, fmt.Errorf("decrypt field value: %w", err)
			}
		}
		if name != "" {
			item.fields[name] = value
		}
	}

	return item, nil
}

func (c *client) prelogin(ctx context.Context) (*preloginResponse, error) {
	body := fmt.Sprintf(`{"email":%q}`, c.email)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/identity/accounts/prelogin", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("prelogin failed (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var result preloginResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result, nil
}

func (c *client) loginWithAPIKey(ctx context.Context) (*tokenResponse, error) {
	data := url.Values{
		"grant_type":       {"client_credentials"},
		"client_id":        {c.clientID},
		"client_secret":    {c.clientSecret},
		"scope":            {"api"},
		"deviceType":       {"14"},
		"deviceIdentifier": {c.deviceID},
		"deviceName":       {"relay-bitwarden"},
	}
	return c.doTokenRequest(ctx, data)
}

func (c *client) doTokenRequest(ctx context.Context, data url.Values) (*tokenResponse, error) {
	resp, err := c.postForm(ctx, c.baseURL+"/identity/connect/token", data)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("login failed (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var tokenResp tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, err
	}

	return &tokenResp, nil
}

func (c *client) fetchProfileKey(ctx context.Context) (string, error) {
	syncResp, err := c.fetchSync(ctx)
	if err != nil {
		return "", err
	}
	if syncResp.Profile.Key == "" {
		return "", fmt.Errorf("bitwarden: profile key is empty")
	}
	return syncResp.Profile.Key, nil
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
