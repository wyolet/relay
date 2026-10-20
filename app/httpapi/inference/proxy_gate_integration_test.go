//go:build integration

// proxy_gate_integration_test.go covers the proxy-mode refusals that need the setting switched on, which only Hydrate can do.
// Run with: make test-integration.

package inference

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// proxyModeCatalog hydrates an empty catalog under pm and returns it with a function that stores and reloads a new value.
func proxyModeCatalog(t *testing.T, pm settings.ProxyMode) (*appcatalog.Catalog, func(settings.ProxyMode)) {
	t.Helper()
	ctx := context.Background()
	pool := storagetest.Pool(t)
	cat, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	set := func(pm settings.ProxyMode) {
		t.Helper()
		raw, err := json.Marshal(pm)
		if err != nil {
			t.Fatalf("marshal settings: %v", err)
		}
		if _, err := stores.Settings.Upsert(ctx, settings.SectionProxyMode, raw); err != nil {
			t.Fatalf("upsert settings: %v", err)
		}
		if _, err := cat.Hydrate(ctx, stores, appcatalog.BootstrapOptions{Pool: pool}); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
	}
	set(pm)
	return cat, set
}

func TestIntegration_AnonymousProxyRefusedBeforeTheBody(t *testing.T) {
	const want = `{"error":{"type":"invalid_request_error","code":"unauthenticated","message":"anonymous proxy traffic is not enabled on this relay"}}`
	cat, _ := proxyModeCatalog(t, settings.ProxyMode{Enabled: true})
	d := buildDeps(t, cat)
	events := make(chan struct{}, 1)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "count", Fn: func(*lifecycle.Context, *lifecycle.PostFlightEvent) (any, error) {
		events <- struct{}{}
		return nil, nil
	}})
	d.Lifecycle = reg
	before := rejectionCount(t, "unauthenticated")

	var reached bool
	w := httptest.NewRecorder()
	gatedEdge(d, &reached).ServeHTTP(w, poisonedProxyRequest(t, ""))

	if w.Code != http.StatusUnauthorized || w.Body.String() != want {
		t.Fatalf("got %d %s, want 401 %s", w.Code, w.Body, want)
	}
	if got := w.Header().Get(HeaderOrigin); got != "relay" {
		t.Fatalf("%s = %q, want relay", HeaderOrigin, got)
	}
	if reached {
		t.Fatal("the refused request reached the handler")
	}
	if got := rejectionCount(t, "unauthenticated") - before; got != 1 {
		t.Fatalf("counter rose by %v, want 1", got)
	}
	select {
	case <-events:
		t.Fatal("a refused request fired a post-flight event")
	case <-time.After(100 * time.Millisecond):
	}
}

// A WebSocket connection outlives the setting it was admitted under: switching proxy mode off refuses the next frame.
func TestIntegration_ProxyModeOffRefusesTheNextFrame(t *testing.T) {
	cat, set := proxyModeCatalog(t, settings.ProxyMode{Enabled: true, AllowUnauthenticated: true})
	d := buildDeps(t, cat)
	srv := httptest.NewServer(ClassifyMiddleware()(PrincipalMiddleware(cat, nil)(wsHandler(d))))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{
		HTTPHeader: http.Header{
			httpheader.HeaderProxyMode: {httpheader.ProxyModeValueProxy},
			"Authorization":            {"Bearer upstream-key"},
		},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	frameCode := func(id string) string {
		t.Helper()
		frame, _ := json.Marshal(map[string]any{"id": id, "payload": json.RawMessage(`{"model":"m","messages":[]}`)})
		if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
		var data string
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("read %s: %v", id, err)
			}
			var f wsFrame
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatalf("decode frame: %v", err)
			}
			if f.ID != id {
				continue
			}
			data += f.Data
			if f.Event == "end" {
				return errCode(t, []byte(data))
			}
		}
	}

	// Admitted: the frame reaches the proxy handler, which wants a pinned host from anonymous callers.
	if got := frameCode("f1"); got != "missing_upstream_host" {
		t.Fatalf("admitted frame code = %q, want missing_upstream_host", got)
	}
	set(settings.ProxyMode{})
	if got := frameCode("f2"); got != "proxy_disabled" {
		t.Fatalf("frame after switch-off code = %q, want proxy_disabled", got)
	}
}
