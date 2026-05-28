package bitwarden

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wyolet/relay/pkg/secret"
)

// Config holds Vaultwarden/Bitwarden connection settings. Secret fields
// (MasterPassword, ClientSecret) must be supplied by the composition layer
// from env — never hard-coded.
type Config struct {
	BaseURL            string
	Email              string
	MasterPassword     string
	ClientID           string
	ClientSecret       string
	SyncInterval       time.Duration
	InsecureSkipVerify bool
	DeviceID           string
}

// Resolver syncs and decrypts vault items into an in-memory cache.
type Resolver struct {
	client *client

	mu       sync.RWMutex
	cache    vaultCache
	syncErr  error
	syncDone bool

	stopCh chan struct{}
}

var _ secret.Resolver = (*Resolver)(nil)

// New constructs a Bitwarden resolver and starts a background sync loop.
func New(cfg Config) *Resolver {
	if cfg.SyncInterval <= 0 {
		cfg.SyncInterval = 5 * time.Minute
	}

	r := &Resolver{
		client: newClient(cfg),
		stopCh: make(chan struct{}),
	}

	go r.syncLoop(cfg.SyncInterval)
	return r
}

func (r *Resolver) syncLoop(interval time.Duration) {
	r.refresh(context.Background())

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.refresh(context.Background())
		case <-r.stopCh:
			return
		}
	}
}

func (r *Resolver) refresh(ctx context.Context) {
	if err := r.client.authenticate(ctx); err != nil {
		r.mu.Lock()
		r.syncErr = err
		r.syncDone = true
		r.mu.Unlock()
		return
	}

	items, err := r.client.sync(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncDone = true
	if err != nil {
		r.syncErr = err
		return
	}
	r.syncErr = nil
	r.cache = newVaultCache(items)
}

// Resolve returns the plaintext value for ref.Path = "<itemNameOrID>[/<field>]".
func (r *Resolver) Resolve(ctx context.Context, ref secret.Ref) ([]byte, error) {
	if ref.Kind != secret.KindBitwarden {
		return nil, fmt.Errorf("bitwarden: wrong kind %q", ref.Kind)
	}

	itemName, field, err := parsePath(ref.Path)
	if err != nil {
		return nil, err
	}

	r.mu.RLock()
	syncDone := r.syncDone
	syncErr := r.syncErr
	cache := r.cache
	r.mu.RUnlock()

	if !syncDone {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		deadline := time.After(30 * time.Second)
		for {
			r.mu.RLock()
			syncDone = r.syncDone
			syncErr = r.syncErr
			cache = r.cache
			r.mu.RUnlock()
			if syncDone {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-deadline:
				return nil, fmt.Errorf("bitwarden: initial sync timed out")
			case <-time.After(50 * time.Millisecond):
			}
		}
	}

	if syncErr != nil {
		return nil, fmt.Errorf("bitwarden: vault sync failed: %w", syncErr)
	}

	item, err := lookupItem(cache, itemName)
	if err != nil {
		return nil, err
	}

	value, err := fieldValue(item, field)
	if err != nil {
		return nil, err
	}
	if value == "" {
		return nil, fmt.Errorf("bitwarden: field %q is empty or missing on item %q", field, itemName)
	}

	return []byte(value), nil
}

func parsePath(path string) (itemName, field string, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", fmt.Errorf("bitwarden: empty path")
	}

	itemName = path
	field = "password"

	if i := strings.Index(path, "/"); i >= 0 {
		itemName = path[:i]
		field = path[i+1:]
		if itemName == "" {
			return "", "", fmt.Errorf("bitwarden: empty item name in path %q", path)
		}
		if field == "" {
			return "", "", fmt.Errorf("bitwarden: empty field in path %q", path)
		}
	}

	return itemName, field, nil
}

func lookupItem(cache vaultCache, nameOrID string) (decryptedItem, error) {
	if matches, ok := cache.byExactName[nameOrID]; ok {
		if len(matches) > 1 {
			return decryptedItem{}, fmt.Errorf("bitwarden: ambiguous item name %q (%d matches)", nameOrID, len(matches))
		}
		return matches[0], nil
	}

	if item, ok := cache.byID[nameOrID]; ok {
		return item, nil
	}

	return decryptedItem{}, fmt.Errorf("bitwarden: item %q not found", nameOrID)
}

func fieldValue(item decryptedItem, field string) (string, error) {
	switch field {
	case "password":
		return item.password, nil
	case "username":
		return item.username, nil
	case "uri":
		return item.uri, nil
	case "notes":
		return item.notes, nil
	default:
		if v, ok := item.fields[field]; ok {
			return v, nil
		}
		return "", fmt.Errorf("bitwarden: unknown field %q on item %q", field, item.name)
	}
}
