package bitwarden

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

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
