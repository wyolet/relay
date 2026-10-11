package scim

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/directory/memstore"
	"github.com/wyolet/relay/auth/principal"
	"github.com/wyolet/relay/auth/rbac"
)

// memTokens is an in-memory TokenStore keyed by token hash.
type memTokens struct {
	mu     sync.Mutex
	byName map[string]*Client
	used   map[string]time.Time
}

func newMemTokens() *memTokens {
	return &memTokens{byName: map[string]*Client{}, used: map[string]time.Time{}}
}

func (m *memTokens) ClientByTokenHash(_ context.Context, hash string) (*Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.byName {
		if c.TokenHash == hash {
			cp := *c
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *memTokens) MarkUsed(_ context.Context, name string, t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.used[name] = t
	return nil
}

func (m *memTokens) CreateClient(_ context.Context, c *Client) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.byName[c.Name]; ok && !old.Revoked {
		return ErrClientExists
	}
	cp := *c
	m.byName[c.Name] = &cp
	return nil
}

func (m *memTokens) ListClients(context.Context) ([]*Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Client
	for _, c := range m.byName {
		cp := *c
		out = append(out, &cp)
	}
	return out, nil
}

func (m *memTokens) RevokeClient(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.byName[name]
	if !ok {
		return ErrNoClient
	}
	c.Revoked = true
	return nil
}

// denyGrants refuses every grant to a caller that is not an admin, so a test sees whether a write ran the check.
type denyGrants struct{ calls int }

func (g *denyGrants) CheckGrant(_ context.Context, p *principal.Principal, _ rbac.Role, _ rbac.Scope, _, _ []string) error {
	g.calls++
	if p == nil || !p.Admin {
		return authz.ErrForbidden
	}
	return nil
}

type revoked struct {
	mu    sync.Mutex
	users []string
}

func (r *revoked) DestroyUser(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users = append(r.users, id)
	return nil
}

type fixture struct {
	t       *testing.T
	dir     *directory.Directory
	stores  directory.Stores
	tokens  *memTokens
	grants  *denyGrants
	ended   *revoked
	h       *Handler
	writes  []Write
	tokenOf map[string]string
}

var admin = &principal.Principal{Admin: true, Kind: principal.KindAdmin}

func newFixture(t *testing.T, opts ...func(*Config)) *fixture {
	t.Helper()
	f := &fixture{t: t, stores: memstore.New().Stores(), tokens: newMemTokens(), grants: &denyGrants{}, ended: &revoked{}, tokenOf: map[string]string{}}
	dir, err := directory.New(directory.Config{Stores: f.stores, Grants: f.grants, Revoker: f.ended})
	if err != nil {
		t.Fatal(err)
	}
	f.dir = dir
	cfg := Config{Directory: dir, Tokens: f.tokens, BaseURL: "https://idp.example/scim/v2",
		OnWrite: func(_ context.Context, w Write) { f.writes = append(f.writes, w) }}
	for _, o := range opts {
		o(&cfg)
	}
	if f.h, err = New(cfg); err != nil {
		t.Fatal(err)
	}
	f.client("entra")
	return f
}

// client creates a SCIM client and remembers its token.
func (f *fixture) client(name string) string {
	f.t.Helper()
	tok, _, err := CreateClient(context.Background(), f.tokens, name)
	if err != nil {
		f.t.Fatal(err)
	}
	f.tokenOf[name] = tok
	return tok
}

type response struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func (f *fixture) do(method, path, body string) response { return f.as("entra", method, path, body) }

func (f *fixture) as(client, method, path, body string) response {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if tok := f.tokenOf[client]; tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	res := response{status: rec.Code, header: rec.Header(), raw: rec.Body.String()}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &res.body); err != nil {
			f.t.Fatalf("%s %s: body is not JSON: %s", method, path, rec.Body.String())
		}
	}
	return res
}

// want fails unless the response has the status.
func (r response) want(t *testing.T, status int) response {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.raw)
	}
	return r
}

func (r response) str(key string) string {
	s, _ := r.body[key].(string)
	return s
}

// createUser posts a user and returns its id.
func (f *fixture) createUser(userName, externalID string) string {
	f.t.Helper()
	body := `{"schemas":["` + schemaUser + `"],"userName":"` + userName + `","externalId":"` + externalID + `","active":true,` +
		`"emails":[{"primary":true,"type":"work","value":"` + userName + `"}]}`
	return f.do(http.MethodPost, "/Users", body).want(f.t, http.StatusCreated).str("id")
}

func (f *fixture) createGroup(displayName string, members ...string) string {
	f.t.Helper()
	ms := make([]string, len(members))
	for i, m := range members {
		ms[i] = `{"value":"` + m + `"}`
	}
	body := `{"schemas":["` + schemaGroup + `"],"displayName":"` + displayName + `","members":[` + strings.Join(ms, ",") + `]}`
	return f.do(http.MethodPost, "/Groups", body).want(f.t, http.StatusCreated).str("id")
}

func (f *fixture) user(id string) *directory.User {
	f.t.Helper()
	u, err := f.stores.Users.Get(context.Background(), id)
	if err != nil || u == nil {
		f.t.Fatalf("user %s: %v, %v", id, u, err)
	}
	return u
}

func (f *fixture) group(id string) *directory.Group {
	f.t.Helper()
	g, err := f.stores.Groups.Get(context.Background(), id)
	if err != nil || g == nil {
		f.t.Fatalf("group %s: %v, %v", id, g, err)
	}
	return g
}
