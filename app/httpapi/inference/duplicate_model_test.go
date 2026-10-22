package inference

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/lifecycle"
)

const duplicateModelBody = `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"model":"other-model"}`

func assertDuplicateModel(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; body %s", w.Code, w.Body)
	}
	if e := parseDispatchErr(t, w.Body.Bytes()); e.Error.Type != "invalid_request_error" || e.Error.Code != "duplicate_model" {
		t.Fatalf("envelope %s/%s, want invalid_request_error/duplicate_model", e.Error.Type, e.Error.Code)
	}
}

// Normal mode routes on encoding/json's last model while a byte-pass may forward both, so the body is refused before routing.
func TestHandleShape_NormalMode_DuplicateModel(t *testing.T) {
	cat, prin := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)
	r := withNormalContext(httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(duplicateModelBody)), prin)
	w := httptest.NewRecorder()
	handleShape(d.Specs.Spec(adapters.OpenAI), d, w, r)
	assertDuplicateModel(t, w)
}

func TestHandleCountTokens_DuplicateModel(t *testing.T) {
	cat, prin := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)
	r := withNormalContext(httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages/count_tokens", strings.NewReader(duplicateModelBody)), prin)
	w := httptest.NewRecorder()
	handleCountTokens(d, w, r)
	assertDuplicateModel(t, w)
}

func TestHandleShape_ProxyMode_DuplicateModel(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++ }))
	defer up.Close()
	cat, _ := proxyLimitCatalog(t, up.URL, true, 100, nil)
	d, _ := proxyLimitDeps(t, cat, nil)
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(duplicateModelBody))
	r.Header.Set(httpheader.HeaderProxyMode, httpheader.ProxyModeValueProxy)
	r.Header.Set(httpheader.HeaderRelayAPIKey, "relay-key")
	r.Header.Set("Authorization", "Bearer upstream-key")
	w := httptest.NewRecorder()
	ClassifyMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleShape(d.Specs.Spec(adapters.Anthropic), d, w, r)
	})).ServeHTTP(w, r)
	assertDuplicateModel(t, w)
	if hits != 0 {
		t.Fatalf("upstream called %d times", hits)
	}
}

// Past the peek window the body streams upstream while it is checked: a second model there aborts the forward with the body incomplete, refunds the reservation and records the refusal, not a success.
func TestHandleProxy_DuplicateModelPastThePeekWindow(t *testing.T) {
	type upstreamSaw struct {
		complete bool
		n        int
	}
	saw := make(chan upstreamSaw, 4)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		saw <- upstreamSaw{complete: err == nil && json.Valid(b), n: len(b)}
		_, _ = w.Write([]byte(`{"id":"m","type":"message","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()

	cat, pr := proxyLimitCatalog(t, up.URL, true, 100, nil)
	d, store := proxyLimitDeps(t, cat, nil)
	kinds := make(chan string, 4)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "kind", Fn: func(_ *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
		kinds <- ev.ErrorKind
		return nil, nil
	}})
	d.Proxy.Lifecycle = reg

	send := func(body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewReader(body))
		ctx := WithClassification(r.Context(), Classification{Mode: ModeProxyAuthed, Key: "relay-key", UpstreamAuth: "Bearer upstream-key", ClientIP: proxyClientIP})
		ctx = withPrincipal(ctx, d.Catalog.Current(), pr, pr.Key)
		ctx = lifecycle.ContextWith(ctx, lifecycle.NewContext("req-dup", "proxy", time.Now()))
		w := httptest.NewRecorder()
		handleProxy(d, w, r.WithContext(ctx), adapters.Anthropic, "test-model")
		return w
	}

	pad := strings.Repeat("a", 2*proxyMaxBodyPeek)
	w := send([]byte(`{"model":"test-model","messages":[{"role":"user","content":"` + pad + `"}],"model":"other-model"}`))
	assertDuplicateModel(t, w)
	select {
	case s := <-saw:
		if s.complete {
			t.Fatalf("upstream received a complete body (%d bytes)", s.n)
		}
	case <-time.After(5 * time.Second):
	}
	select {
	case k := <-kinds:
		if k != "duplicate_model" {
			t.Fatalf("recorded as %q, want duplicate_model", k)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no usage event for the refusal")
	}

	// Settled as cancelled (commit ARGV[7]), so no tokens are booked against the policy.
	commits := store.Named("limit.commit")
	if len(commits) != 1 || len(commits[0].Args) < 7 || commits[0].Args[6] != int64(1) {
		t.Fatalf("reservation settlement: %+v, want one cancelled commit", commits)
	}

	if w := send([]byte(`{"model":"test-model","messages":[{"role":"user","content":"` + pad + `"}]}`)); w.Code != http.StatusOK {
		t.Fatalf("clean request after the refusal: status %d body %s", w.Code, w.Body)
	}
	if s := <-saw; !s.complete {
		t.Fatalf("clean body arrived incomplete (%d bytes)", s.n)
	}
}
