package session

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"

	"github.com/wyolet/relay/pkg/kv"
)

// Sessions issued before a deploy must keep working after it, so the cookie, the kv keys and the stored payload are pinned literally.
func TestLayout_Mem(t *testing.T) {
	runLayoutSuite(t, func(*testing.T) kv.Store { return kv.NewMem() })
}

func runLayoutSuite(t *testing.T, newStore func(*testing.T) kv.Store) {
	t.Run("password login", func(t *testing.T) {
		for _, secure := range []bool{false, true} {
			store := newStore(t)
			m := New(store, secure, "sess:")
			c := loginWith(t, m, func(ctx context.Context) error { return m.Login(ctx, "u-1", "alice", "admin") })
			checkCookie(t, c, secure)
			checkKeys(t, store, c.Value, "u-1", map[string]any{
				"user_id":  "u-1",
				"username": "alice",
				"roles":    `["admin"]`,
			})
		}
	})
	t.Run("oidc login", func(t *testing.T) {
		store := newStore(t)
		m := New(store, true, "sess:")
		c := loginWith(t, m, func(ctx context.Context) error {
			return m.LoginOIDC(ctx, "u-2", "bob", "https://idp|sub-2", "sid-9", []string{"eng"}, "viewer")
		})
		checkCookie(t, c, true)
		checkKeys(t, store, c.Value, "u-2", map[string]any{
			"user_id":      "u-2",
			"username":     "bob",
			"roles":        `["viewer"]`,
			"idp_groups":   `["eng"]`,
			"oidc_subject": "https://idp|sub-2",
			"oidc_sid":     "sid-9",
		})
	})
	t.Run("logout clears cookie and session", func(t *testing.T) {
		store := newStore(t)
		m := New(store, false, "sess:")
		c := loginWith(t, m, func(ctx context.Context) error { return m.Login(ctx, "u-1", "alice") })
		h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := m.Logout(r.Context()); err != nil {
				t.Fatalf("Logout: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
		req.AddCookie(c)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("logout status = %d, want 204", rec.Code)
		}
		cleared := rec.Result().Cookies()
		if len(cleared) != 1 || cleared[0].Name != "relay_session" || cleared[0].Value != "" || cleared[0].MaxAge != -1 {
			t.Fatalf("logout cookie = %+v, want relay_session cleared", cleared)
		}
		if _, err := store.Get(context.Background(), "sess:"+c.Value); err == nil {
			t.Error("session key survived logout")
		}
	})
	t.Run("destroy user removes its keys", func(t *testing.T) {
		store := newStore(t)
		m := New(store, false, "sess:")
		c := loginWith(t, m, func(ctx context.Context) error { return m.Login(ctx, "u-1", "alice") })
		if err := m.DestroyUser(context.Background(), "u-1"); err != nil {
			t.Fatalf("DestroyUser: %v", err)
		}
		if got := keysUnder(t, store); len(got) != 0 {
			t.Errorf("keys left after DestroyUser: %v (cookie %s)", got, c.Value)
		}
	})
}

func loginWith(t *testing.T, m *Manager, login func(context.Context) error) *http.Cookie {
	t.Helper()
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := login(r.Context()); err != nil {
			t.Fatalf("login: %v", err)
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login set %d cookies, want 1: %v", len(cookies), cookies)
	}
	return cookies[0]
}

func checkCookie(t *testing.T, c *http.Cookie, secure bool) {
	t.Helper()
	if c.Name != "relay_session" || c.Path != "/" || c.Domain != "" || !c.HttpOnly || c.Secure != secure || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie attributes = %+v, want relay_session Path=/ HttpOnly Secure=%v SameSite=Strict", c, secure)
	}
	// scs rounds Max-Age up to the next second.
	if c.MaxAge < 86400 || c.MaxAge > 86401 {
		t.Errorf("cookie Max-Age = %d, want 24h", c.MaxAge)
	}
	if d := time.Until(c.Expires); d < 24*time.Hour-time.Minute || d > 24*time.Hour+time.Minute {
		t.Errorf("cookie Expires in %s, want 24h", d)
	}
}

// checkKeys asserts the login wrote exactly the session key and the user-index key, with the stored payload decoding to want.
func checkKeys(t *testing.T, store kv.Store, token, userID string, want map[string]any) {
	t.Helper()
	sessKey, indexKey := "sess:"+token, "sess:user:"+userID+":"+token
	wantKeys := []string{sessKey, indexKey}
	slices.Sort(wantKeys)
	if got := keysUnder(t, store); !slices.Equal(got, wantKeys) {
		t.Errorf("keys = %v, want %v", got, wantKeys)
	}
	ctx := context.Background()
	if v, err := store.Get(ctx, indexKey); err != nil || string(v) != token {
		t.Errorf("index value = %q, %v; want the token", v, err)
	}
	raw, err := store.Get(ctx, sessKey)
	if err != nil {
		t.Fatalf("session key: %v", err)
	}
	var entry struct {
		Data   []byte    `json:"d"`
		Expiry time.Time `json:"e"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("session value is not {d,e} json: %v (%s)", err, raw)
	}
	if d := time.Until(entry.Expiry); d < 24*time.Hour-time.Minute || d > 24*time.Hour+time.Minute {
		t.Errorf("stored expiry in %s, want 24h", d)
	}
	_, values, err := scs.GobCodec{}.Decode(entry.Data)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if !maps.Equal(values, want) {
		t.Errorf("payload = %v, want %v", values, want)
	}
}

func keysUnder(t *testing.T, store kv.Store) []string {
	t.Helper()
	entries, err := store.Range(context.Background(), "sess:")
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		keys = append(keys, e.Key)
	}
	slices.Sort(keys)
	return keys
}
