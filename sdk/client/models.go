package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/wyolet/relay/sdk/catalog"
)

// Origin names where a Models or Catalog result came from.
type Origin string

const (
	// OriginRelay is a result the relay server returned for the client's credential: only what that credential can route to, from the deployment's live catalog.
	OriginRelay Origin = "relay"
	// OriginCatalog is the latest public catalog from the catalog server, read by a client that doesn't go through relay. It is what the catalog says the host serves, not what the provider key can call.
	OriginCatalog Origin = "catalog"
	// OriginEmbedded is the public catalog compiled into this SDK. A relay client gets it when relay could not be reached and no earlier relay copy is held; it is not scoped to the credential and may list models the relay doesn't serve. Any other client gets it when the catalog server is off or unreachable, or its latest release is older than the embed.
	OriginEmbedded Origin = "embedded"
)

// ModelList is the model ids a client can use: for a relay client those its credential can use, in the order relay lists them; for any other client those its catalog host serves, in catalog order.
type ModelList struct {
	IDs    []string
	Origin Origin
	// Stale marks the last good copy, served because a refresh failed.
	Stale bool
	// Err is the fetch error behind Stale; nil when the list is fresh.
	Err error
}

// ModelCatalog is a catalog for building a model picker or computing cost.
type ModelCatalog struct {
	*catalog.IndexedCatalog
	Origin Origin
	// Stale marks the last good copy, served because a refresh failed.
	Stale bool
	// Err is the fetch error behind Stale or an embedded fallback; nil when the catalog is fresh.
	Err error
}

// maxListBytes caps a /v1/models or /v1/catalog body; the full public catalog is well under 1 MiB.
const maxListBytes = 32 << 20

// relayCache holds a relay client's last good results. A non-nil one is also what marks a client as a relay client.
type relayCache struct {
	mu      sync.Mutex
	ids     []string
	hasIDs  bool
	etag    string
	catalog *catalog.IndexedCatalog
}

func (r *relayCache) drop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids, r.hasIDs, r.etag, r.catalog = nil, false, "", nil
}

// Models returns the model ids relay lists for the client's credential (GET /v1/models). When the refresh fails on a network error or a 5xx, the last good list is returned with Stale and Err set; with none held, the error is returned, since the embedded catalog's ids are not the credential's. A 401 or 403 returns an error and forgets the held list.
//
// A client that doesn't go through relay returns the binding slugs of Catalog instead, with its Origin, Stale and Err.
func (c *Client) Models(ctx context.Context) (*ModelList, error) {
	if c.relay == nil {
		return c.publicModels(ctx)
	}
	if c.configErr != nil {
		return nil, c.configErr
	}
	status, body, _, err := c.relayGet(ctx, "/v1/models", "")
	if err == nil && status == http.StatusOK {
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err = json.Unmarshal(body, &list); err == nil {
			ids := make([]string, len(list.Data))
			for i, m := range list.Data {
				ids[i] = m.ID
			}
			c.relay.mu.Lock()
			c.relay.ids, c.relay.hasIDs = ids, true
			c.relay.mu.Unlock()
			return &ModelList{IDs: append([]string(nil), ids...), Origin: OriginRelay}, nil
		}
		err = fmt.Errorf("relay client: decode /v1/models: %w", err)
	}
	if err == nil {
		if fatal := c.relayStatusError(status, body); fatal != nil {
			return nil, fatal
		}
		err = parseAPIError(c.host(), status, body)
	}
	c.relay.mu.Lock()
	ids, ok := c.relay.ids, c.relay.hasIDs
	c.relay.mu.Unlock()
	if !ok {
		return nil, err
	}
	return &ModelList{IDs: append([]string(nil), ids...), Origin: OriginRelay, Stale: true, Err: err}, nil
}

// Catalog returns the catalog relay serves for the client's credential (GET /v1/catalog): the models, hosts and prices it can route to, with no upstream addresses. The last good copy is kept in memory and revalidated with its ETag. When the refresh fails on a network error or a 5xx, the last good copy is returned with Stale and Err set; with none held, the embedded catalog is returned with Origin embedded and Err set. A 401 or 403 returns an error and forgets the held copy; a 404 means the relay predates /v1/catalog and returns an error.
//
// A client that doesn't go through relay reads the latest public catalog from its catalog source (WithCatalogSource) and sends nothing to its base URL or to relay. A client built by For, ForFrom or ForMode gets only its catalog host, with the models and providers that host's bindings reference; a host the fetched catalog lacks gives an empty catalog. A client built by a constructor (OpenAI, Anthropic, New, ...) has no catalog host and gets the whole catalog.
func (c *Client) Catalog(ctx context.Context) (*ModelCatalog, error) {
	if c.relay == nil {
		return c.publicCatalog(ctx)
	}
	if c.configErr != nil {
		return nil, c.configErr
	}
	c.relay.mu.Lock()
	etag, held := c.relay.etag, c.relay.catalog
	c.relay.mu.Unlock()

	status, body, newETag, err := c.relayGet(ctx, "/v1/catalog", etag)
	if err == nil {
		switch {
		case status == http.StatusNotModified && held != nil:
			return &ModelCatalog{IndexedCatalog: held, Origin: OriginRelay}, nil
		case status == http.StatusOK:
			ic, loadErr := catalog.LoadBytes(body)
			if loadErr == nil {
				c.relay.mu.Lock()
				c.relay.etag, c.relay.catalog = newETag, ic
				c.relay.mu.Unlock()
				return &ModelCatalog{IndexedCatalog: ic, Origin: OriginRelay}, nil
			}
			err = fmt.Errorf("relay client: /v1/catalog: %w", loadErr)
		case status == http.StatusNotFound:
			return nil, fmt.Errorf("relay client: this relay does not serve /v1/catalog: %w", parseAPIError(c.host(), status, body))
		default:
			if fatal := c.relayStatusError(status, body); fatal != nil {
				return nil, fatal
			}
			err = parseAPIError(c.host(), status, body)
		}
	}
	if held != nil {
		return &ModelCatalog{IndexedCatalog: held, Origin: OriginRelay, Stale: true, Err: err}, nil
	}
	embedded, loadErr := catalog.Load()
	if loadErr != nil {
		return nil, errors.Join(err, loadErr)
	}
	return &ModelCatalog{IndexedCatalog: embedded, Origin: OriginEmbedded, Err: err}, nil
}

// relayStatusError is the error for a status that must not fall back to a held copy: a refused credential, whose earlier results may no longer be its to see.
func (c *Client) relayStatusError(status int, body []byte) error {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return nil
	}
	c.relay.drop()
	return parseAPIError(c.host(), status, body)
}

// relayGet GETs path on the relay with the client's credential and headers. The transport negotiates and inflates gzip itself.
func (c *Client) relayGet(ctx context.Context, path, ifNoneMatch string) (status int, body []byte, etag string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpURL(c.baseURL)+path, nil)
	if err != nil {
		return 0, nil, "", err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	c.applyAuth(req.Header)
	req.Header.Set("Accept", "application/json")
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxListBytes+1))
	if err != nil {
		return 0, nil, "", err
	}
	if len(body) > maxListBytes {
		return 0, nil, "", fmt.Errorf("relay client: %s response exceeds %d bytes", path, maxListBytes)
	}
	return resp.StatusCode, body, resp.Header.Get("ETag"), nil
}

// httpURL maps a WebSocket base URL (RelayWS accepts either form) to the HTTP base serving the same relay.
func httpURL(base string) string {
	switch {
	case strings.HasPrefix(base, "wss://"):
		return "https://" + strings.TrimPrefix(base, "wss://")
	case strings.HasPrefix(base, "ws://"):
		return "http://" + strings.TrimPrefix(base, "ws://")
	default:
		return base
	}
}
