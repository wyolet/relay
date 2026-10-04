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
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/pkg/slug"
)

// wsFrame mirrors the transport's outbound frame; the transport keeps its
// own type unexported.
type wsFrame struct {
	ID     string `json:"id"`
	Event  string `json:"event"`
	Status int    `json:"status,omitempty"`
	Data   string `json:"data,omitempty"`
}

// A connection outlives many catalog reloads. TestWSFrameReadsTheLiveSnapshot
// pins that a change landing between two frames is visible to the second:
// the model the first frame cannot find resolves for the second.
func TestWSFrameReadsTheLiveSnapshot(t *testing.T) {
	cat, pr := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)
	pinned := cat.Current()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// What the middleware chain leaves on the upgrade request: the
		// resolved principal and the snapshot it resolved against.
		ctx := WithClassification(r.Context(), Classification{Mode: ModeNormal})
		ctx = context.WithValue(ctx, ctxKeyT{}, pr.Key)
		ctx = context.WithValue(ctx, ctxPrincipalT{}, pr)
		ctx = context.WithValue(ctx, ctxSnapshotT{}, pinned)
		wsHandler(d)(w, r.WithContext(ctx))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	send := func(id string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"id":      id,
			"payload": json.RawMessage(`{"model":"late-model","messages":[]}`),
		})
		if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
	}
	// The error body arrives as chunk data; read until this request's end.
	readCode := func(id string) string {
		t.Helper()
		var payload string
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
			payload += f.Data
			if f.Event == "end" {
				break
			}
		}
		var e errBody
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			t.Fatalf("decode error body %q: %v", payload, err)
		}
		return e.Error.Code
	}

	send("f1")
	if code := readCode("f1"); code != "model_not_found" {
		t.Fatalf("first frame code = %q, want model_not_found", code)
	}

	// Rename the model row in place (same id, so the reconciler patches the
	// live snapshot rather than reloading from the stores).
	renamed := *pinned.AllModels()[0]
	renamed.Meta.Name = "late-model"
	renamed.Spec.Snapshots = []model.Snapshot{{Name: slug.From("late-model")}}
	renamed.Spec.Pointer = slug.From("late-model")
	if err := cat.ApplyModelUpsert(&renamed); err != nil {
		t.Fatalf("upsert model: %v", err)
	}
	if cat.Current() == pinned {
		t.Fatal("catalog did not publish a new snapshot")
	}

	send("f2")
	// Resolvable now, and refused for a different reason — the second frame
	// read the snapshot the reload published, not the one pinned at upgrade.
	if code := readCode("f2"); code != "translate_request" {
		t.Fatalf("second frame code = %q, want translate_request", code)
	}
}
