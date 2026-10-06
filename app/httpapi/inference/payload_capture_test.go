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

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/lifecycle"
)

const captureKeyPlaintext = "sk-wr-capture"

// captureCase sets the two payload flags; a nil policyCaptures leaves the key with no policy.
type captureCase struct {
	name           string
	policyCaptures *bool
	keyCaptures    bool
	want           bool
}

func captureCases() []captureCase {
	on, off := true, false
	return []captureCase{
		{"policy on, key off", &on, false, true},
		{"policy off, key on", &off, true, false},
		{"no policy, key on", nil, true, true},
		{"no policy, key off", nil, false, false},
	}
}

func governedCaptureCases() []captureCase {
	var out []captureCase
	for _, c := range captureCases() {
		if c.policyCaptures != nil {
			out = append(out, c)
		}
	}
	return out
}

// captureCatalog is the dispatch fixture with its host keyless and pointed at upstreamURL, and its key authenticating as captureKeyPlaintext with the flags c sets.
func captureCatalog(t *testing.T, upstreamURL string, c captureCase) *catalog.Catalog {
	t.Helper()
	cat, pr := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	h := *cat.Current().Hosts()[0]
	h.Spec = host.Spec{BaseURL: upstreamURL, NoAuth: true}
	if err := cat.ApplyHostUpsert(&h); err != nil {
		t.Fatalf("host upsert: %v", err)
	}
	k := *pr.Key
	userID := meta.NewID()
	k.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: userID}
	k.Spec.Principal = key.Principal{Kind: key.PrincipalUser, ID: userID}
	k.Spec.KeyHash = sha(captureKeyPlaintext)
	k.Spec.PassthroughAllowed = true
	k.Spec.PayloadLoggingEnabled = c.keyCaptures
	if c.policyCaptures == nil {
		k.Spec.PolicyID = ""
	} else {
		// A keyless host needs no host keys; dropping them lets the row pass validation as a system policy.
		pol := *pr.Policy
		pol.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem}
		pol.Spec.HostKeyIDs = nil
		pol.Spec.PayloadLoggingEnabled = *c.policyCaptures
		if err := cat.ApplyPolicyUpsert(&pol); err != nil {
			t.Fatalf("policy upsert: %v", err)
		}
	}
	if err := cat.ApplyKeyUpsert(&k); err != nil {
		t.Fatalf("key upsert: %v", err)
	}
	return cat
}

// captureDeps wires runnable deps whose post-flight hook reports each request's capture gate.
func captureDeps(t *testing.T, cat *catalog.Catalog) (Deps, <-chan bool) {
	t.Helper()
	d := buildRunnableDeps(t, cat)
	gates := make(chan bool, 4)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "capture-gate", Fn: func(lc *lifecycle.Context, _ *lifecycle.PostFlightEvent) (any, error) {
		gates <- lc.PayloadLog
		return nil, nil
	}})
	d.Lifecycle = reg
	d.Pipeline.Lifecycle = reg
	d.Proxy = proxy.New(nil, reg, nil)
	return d, gates
}

// captureEdge serves the shape route behind the same classification and credential middleware inference mounts.
func captureEdge(d Deps) http.Handler {
	spec := d.Specs.Spec(adapters.OpenAI)
	return ClassifyMiddleware()(PrincipalMiddleware(d.Catalog, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleShape(spec, d, w, r)
	})))
}

func okUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func awaitGate(t *testing.T, gates <-chan bool) bool {
	t.Helper()
	select {
	case g := <-gates:
		return g
	case <-time.After(5 * time.Second):
		t.Fatal("post-flight hook never fired")
		return false
	}
}

func captureBodyFor(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
}

var captureBody = captureBodyFor("test-model")

// postThroughPipeline sends one normal-mode request authenticated by plaintext and fails unless the upstream answered.
func postThroughPipeline(t *testing.T, d Deps, plaintext, body string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+plaintext)
	w := httptest.NewRecorder()
	captureEdge(d).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

// sendWSFrame opens a WebSocket authenticated by plaintext, sends body as one frame and waits for that frame's end.
func sendWSFrame(t *testing.T, d Deps, plaintext, body string) {
	t.Helper()
	srv := httptest.NewServer(ClassifyMiddleware()(PrincipalMiddleware(d.Catalog, nil)(wsHandler(d))))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + plaintext}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	frame, _ := json.Marshal(map[string]any{"id": "f1", "payload": json.RawMessage(body)})
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var f wsFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		if f.ID == "f1" && f.Event == "end" {
			return
		}
	}
}

// A request under a policy is captured exactly when the policy says so, whatever the key's flag. The no-policy cases need the policy-less flow switched on, which only Postgres can do (payload_capture_integration_test.go).
func TestPayloadCapture_Pipeline(t *testing.T) {
	for _, c := range governedCaptureCases() {
		t.Run(c.name, func(t *testing.T) {
			d, gates := captureDeps(t, captureCatalog(t, okUpstream(t).URL, c))
			postThroughPipeline(t, d, captureKeyPlaintext, captureBody)
			if got := awaitGate(t, gates); got != c.want {
				t.Fatalf("captured = %v, want %v", got, c.want)
			}
		})
	}
}

// Proxy mode reaches the same decision; the request stops at the proxy-mode switch, after the gate is set.
func TestPayloadCapture_ProxyAuthed(t *testing.T) {
	for _, c := range captureCases() {
		t.Run(c.name, func(t *testing.T) {
			d, gates := captureDeps(t, captureCatalog(t, okUpstream(t).URL, c))
			r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(captureBody))
			r.Header.Set(httpheader.HeaderProxyMode, httpheader.ProxyModeValueProxy)
			r.Header.Set(httpheader.HeaderRelayAPIKey, captureKeyPlaintext)
			r.Header.Set("Authorization", "Bearer upstream-key")
			w := httptest.NewRecorder()
			captureEdge(d).ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status %d, want the proxy-mode switch's 403: %s", w.Code, w.Body.String())
			}
			if got := awaitGate(t, gates); got != c.want {
				t.Fatalf("captured = %v, want %v", got, c.want)
			}
		})
	}
}

// Anonymous proxy traffic carries no credential, so nothing opts it in.
func TestPayloadCapture_ProxyAnonymous(t *testing.T) {
	on := true
	d, gates := captureDeps(t, captureCatalog(t, okUpstream(t).URL, captureCase{policyCaptures: &on, keyCaptures: true}))
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(captureBody))
	r.Header.Set(httpheader.HeaderProxyMode, httpheader.ProxyModeValueProxy)
	r.Header.Set("Authorization", "Bearer upstream-key")
	captureEdge(d).ServeHTTP(httptest.NewRecorder(), r)
	if awaitGate(t, gates) {
		t.Fatal("anonymous proxy request captured")
	}
}

// Each WebSocket frame re-resolves the key and its policy and takes the same decision. The frame stops at the stub translator, after the gate is set. No-policy cases: payload_capture_integration_test.go.
func TestPayloadCapture_WebSocketFrame(t *testing.T) {
	for _, c := range governedCaptureCases() {
		t.Run(c.name, func(t *testing.T) {
			d, gates := captureDeps(t, captureCatalog(t, okUpstream(t).URL, c))
			sendWSFrame(t, d, captureKeyPlaintext, captureBody)
			if got := awaitGate(t, gates); got != c.want {
				t.Fatalf("captured = %v, want %v", got, c.want)
			}
		})
	}
}
