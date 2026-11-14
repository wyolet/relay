package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/auth/apikey"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/lifecycle"
)

const captureKeyPlaintext = "sk-wr-capture"

// captureCase sets the key's own payload flag.
type captureCase struct {
	keyCaptures bool
}

// rekey changes the fixture key of a buildDispatchCatalog catalog in the plans and the catalog, and refreshes the principal the test holds.
func rekey(t *testing.T, cat *catalog.Catalog, mutate func(*key.Key)) {
	t.Helper()
	f := fixtureOf(t, cat)
	k := *f.key
	mutate(&k)
	plantest.Apply(t, f.plans, &k)
	if err := cat.ApplyKeyUpsert(&k); err != nil {
		t.Fatalf("key upsert: %v", err)
	}
	f.key = &k
	*f.principal = *f.resolve(t)
}

// captureCatalog is the dispatch fixture pointed at upstreamURL, with its key authenticating as captureKeyPlaintext, allowed to bring its own upstream key, and capturing as c says.
func captureCatalog(t *testing.T, upstreamURL string, c captureCase) *catalog.Catalog {
	t.Helper()
	cat, _ := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	pointHostAt(t, cat, upstreamURL)
	rekey(t, cat, func(k *key.Key) {
		k.Spec.KeyHash = apikey.Hash(captureKeyPlaintext)
		k.Spec.PassthroughAllowed = true
		k.Spec.PayloadLoggingEnabled = c.keyCaptures
	})
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

func okUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// decodeEnvelope decodes body as relay's error envelope, failing on a body that is anything else.
func decodeEnvelope(t *testing.T, body []byte) httpapi.APIErrorBody {
	t.Helper()
	var env httpapi.APIError
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("body %q is not an error envelope: %v", body, err)
	}
	return env.Err
}

func envelopeOf(t *testing.T, rec *httptest.ResponseRecorder) httpapi.APIErrorBody {
	t.Helper()
	return decodeEnvelope(t, rec.Body.Bytes())
}

func errCode(t *testing.T, body []byte) string {
	t.Helper()
	return decodeEnvelope(t, body).Code
}

// wsFrame mirrors the transport's outbound frame; the transport keeps its own type unexported.
type wsFrame struct {
	ID     string `json:"id"`
	Event  string `json:"event"`
	Status int    `json:"status,omitempty"`
	Data   string `json:"data,omitempty"`
}

// wsConn dials wsHandler(d) over an upgrade request carrying what the middleware chain leaves on it: pr and the snapshot it resolved against.
func wsConn(t *testing.T, d Deps, pr *Principal) (*websocket.Conn, context.Context) {
	t.Helper()
	snap := d.Catalog.Current()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(withNormalContext(r, pr).Context(), ctxSnapshotT{}, snap)
		wsHandler(d)(w, r.WithContext(ctx))
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	t.Cleanup(func() {
		if conn != nil {
			conn.Close(websocket.StatusNormalClosure, "")
		}
		cancel()
		srv.Close()
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn, ctx
}

// wsExchange sends payload as the frame id and returns the status and body of its response.
func wsExchange(t *testing.T, ctx context.Context, conn *websocket.Conn, id, payload string) (status int, body string) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"id":"`+id+`","payload":`+payload+`}`)); err != nil {
		t.Fatalf("write %s: %v", id, err)
	}
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		var f wsFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("decode frame %q: %v", raw, err)
		}
		if f.ID != id {
			t.Fatalf("frame for %q while waiting on %q", f.ID, id)
		}
		body += f.Data
		if f.Event == "end" {
			return f.Status, body
		}
	}
}

// wantAnswer checks a relay-minted error: its status, its whole envelope, and the named headers (an empty value means absent).
func wantAnswer(t *testing.T, rec *httptest.ResponseRecorder, status int, want httpapi.APIErrorBody, headers map[string]string) {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status = %d, want %d", rec.Code, status)
	}
	if diff := cmp.Diff(want, envelopeOf(t, rec)); diff != "" {
		t.Errorf("envelope (-want +got):\n%s", diff)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	for k, v := range headers {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
}

// dispatch runs one normal-mode request as pr through Dispatch.
func dispatch(d Deps, pr *Principal, in DispatchInput) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	Dispatch(d, w, withNormalContext(httptest.NewRequest(http.MethodPost, "/", nil), pr), in)
	return w
}

// outcome is what a finalized request's post-flight event says happened.
type outcome struct {
	Status        int
	Kind, Refusal string
	Message       string
}

// watchOutcomes makes d's dispatch and pipeline report every finalized request's outcome.
func watchOutcomes(d *Deps) <-chan outcome {
	got := make(chan outcome, 8)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "outcome", Fn: func(_ *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
		got <- outcome{ev.Status, ev.ErrorKind, ev.Refusal, ev.ErrorMessage}
		return nil, nil
	}})
	d.Lifecycle = reg
	if d.Pipeline != nil {
		d.Pipeline.Lifecycle = reg
	}
	return got
}

// wantOutcome checks the next finalized request's outcome, and that it is the only one.
func wantOutcome(t *testing.T, got <-chan outcome, want outcome) {
	t.Helper()
	select {
	case o := <-got:
		if diff := cmp.Diff(want, o); diff != "" {
			t.Errorf("post-flight outcome (-want +got):\n%s", diff)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no post-flight event")
	}
	select {
	case o := <-got:
		t.Errorf("a second post-flight event fired: %+v", o)
	case <-time.After(20 * time.Millisecond):
	}
}

// waitForFreeSlot waits for the detached post-flight to hand the admission slot back.
func waitForFreeSlot(t *testing.T, adm *httpapi.Admission) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		probe := lifecycle.NewContext("probe", "pipeline", time.Now())
		if adm.PreFlight(context.Background(), probe, &lifecycle.PreFlightEvent{}) == nil {
			adm.Collect(probe)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("admission slot never released")
}
