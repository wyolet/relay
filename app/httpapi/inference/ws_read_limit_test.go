package inference

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wyolet/relay/app/adapters"
)

// A frame carries one request body, so a body the HTTP routes accept must fit in a frame too.
func TestWSFrameAcceptsBodiesUpToTheRequestLimit(t *testing.T) {
	cat, pr := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	payload := `{"model":"missing-model","messages":[],"pad":"` + strings.Repeat("x", 2<<20) + `"}`

	dial := func(t *testing.T, maxBody int64) (*websocket.Conn, context.Context) {
		t.Helper()
		d := buildDeps(t, cat)
		d.MaxRequestBytes = maxBody
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := WithClassification(r.Context(), Classification{Mode: ModeNormal})
			ctx = context.WithValue(ctx, ctxKeyT{}, pr.Key)
			ctx = context.WithValue(ctx, ctxPrincipalT{}, pr)
			ctx = context.WithValue(ctx, ctxSnapshotT{}, cat.Current())
			wsHandler(d)(w, r.WithContext(ctx))
		}))
		t.Cleanup(srv.Close)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
		frame, _ := json.Marshal(map[string]any{"id": "big", "payload": json.RawMessage(payload)})
		if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
			t.Fatalf("write: %v", err)
		}
		return conn, ctx
	}

	t.Run("within the limit", func(t *testing.T) {
		conn, ctx := dial(t, 32<<20)
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var f wsFrame
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatalf("decode frame: %v", err)
			}
			if f.ID == "big" && f.Event == "end" {
				return
			}
		}
	})

	t.Run("over the limit", func(t *testing.T) {
		conn, ctx := dial(t, 1<<20)
		_, _, err := conn.Read(ctx)
		var ce websocket.CloseError
		if !errors.As(err, &ce) || ce.Code != websocket.StatusMessageTooBig {
			t.Fatalf("read after an oversized frame: %v, want close %d", err, websocket.StatusMessageTooBig)
		}
	})
}
