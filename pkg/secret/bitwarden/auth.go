package bitwarden

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

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
