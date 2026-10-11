package scim

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/wyolet/relay/auth/apikey"
	"github.com/wyolet/relay/auth/directory"
)

// TokenPrefix starts every client token, so a leaked one is recognisable in logs and secret scanners.
const TokenPrefix = "scim_"

var (
	// ErrClientExists reports a create for a name a live client holds.
	ErrClientExists = errors.New("scim: client exists")
	// ErrNoClient reports an operation on a client that does not exist.
	ErrNoClient = errors.New("scim: no such client")
)

// Client is one provisioning source with its bearer token. Its name is the source stamped on every row it creates.
type Client struct {
	Name string
	// TokenHash is apikey.Hash of the token; the token itself is never stored.
	TokenHash string
	// TokenPrefix is the start of the token, for telling tokens apart in a list.
	TokenPrefix string
	CreatedAt   time.Time
	// LastUsedAt is when the token last authenticated; zero for never.
	LastUsedAt time.Time
	Revoked    bool
}

// TokenStore keeps the clients. Lookups go by token hash, so finding a client costs the same whatever the token.
type TokenStore interface {
	// ClientByTokenHash returns the client whose token hashes to hash, revoked or not, or (nil, nil).
	ClientByTokenHash(ctx context.Context, hash string) (*Client, error)
	// MarkUsed records that the client authenticated at t. A store may skip writes closer together than it cares to keep.
	MarkUsed(ctx context.Context, name string, t time.Time) error
	// CreateClient stores c. A name a live client holds is ErrClientExists; a revoked client's name is issued again, so the new token takes over the rows the old one provisioned.
	CreateClient(ctx context.Context, c *Client) error
	// ListClients returns every client ordered by name.
	ListClients(ctx context.Context) ([]*Client, error)
	// RevokeClient revokes the client's token for good; a missing name is ErrNoClient, and revoking twice is not an error.
	RevokeClient(ctx context.Context, name string) error
}

// CreateClient mints a token for a new client named name and stores the client. The token is returned once and kept nowhere. The name must be a DNS-1123 label, as it is stamped on every row the client writes.
func CreateClient(ctx context.Context, store TokenStore, name string) (token string, c *Client, err error) {
	if !directory.ValidName(name) {
		return "", nil, fmt.Errorf("scim: client name %q must be a DNS-1123 label of at most %d characters", name, directory.MaxNameLen)
	}
	g, err := apikey.Generate(TokenPrefix)
	if err != nil {
		return "", nil, err
	}
	c = &Client{Name: name, TokenHash: g.Hash, TokenPrefix: g.Prefix}
	if err := store.CreateClient(ctx, c); err != nil {
		return "", nil, err
	}
	return g.Plaintext, c, nil
}

type clientKey struct{}

// ClientFrom returns the name of the client the request authenticated as, for code running under the handler.
func ClientFrom(ctx context.Context) (string, bool) {
	name, ok := ctx.Value(clientKey{}).(string)
	return name, ok
}

var errUnauthorized = errorf(http.StatusUnauthorized, "", "a valid bearer token is required")

// authenticate finds the live client behind the request's bearer token.
func (h *Handler) authenticate(r *http.Request) (*Client, error) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || !strings.HasPrefix(token, TokenPrefix) {
		return nil, errUnauthorized
	}
	hash := apikey.Hash(strings.TrimSpace(token))
	c, err := h.cfg.Tokens.ClientByTokenHash(r.Context(), hash)
	if err != nil {
		return nil, fmt.Errorf("scim: look up client: %w", err)
	}
	// The store matched on the hash already; comparing again in constant time keeps a store that matches loosely from admitting a near miss.
	if c == nil || c.Revoked || subtle.ConstantTimeCompare([]byte(c.TokenHash), []byte(hash)) != 1 {
		return nil, errUnauthorized
	}
	if err := h.cfg.Tokens.MarkUsed(r.Context(), c.Name, time.Now()); err != nil {
		h.log.Warn("scim: record client use", "client", c.Name, "err", err)
	}
	return c, nil
}
