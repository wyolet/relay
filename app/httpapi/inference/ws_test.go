package inference

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/slug"
)

// Without the canonical shape there is nothing to serve frames in; the refusal must come before the upgrade, while an HTTP error can still be written.
func TestWSHandler_NoCanonicalSpec(t *testing.T) {
	cat, pr := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)
	useSpecs(&d, adapter.NewRegistry())

	rec := httptest.NewRecorder()
	wsHandler(d)(rec, withNormalContext(httptest.NewRequest(http.MethodGet, "/v1/ws", nil), pr))
	wantAnswer(t, rec, http.StatusInternalServerError, httpapi.APIErrorBody{Type: "server_error", Code: "no_spec", Message: "canonical adapter not registered"}, nil)
}

// A connection outlives many catalog reloads: a change landing between two frames is visible to the second.
func TestWSFrameReadsTheLiveSnapshot(t *testing.T) {
	cat, pr := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)
	pinned := cat.Current()
	conn, ctx := wsConn(t, d, pr)
	const payload = `{"model":"late-model","messages":[]}`

	if status, body := wsExchange(t, ctx, conn, "f1", payload); status != http.StatusNotFound || errCode(t, []byte(body)) != "model_not_found" {
		t.Fatalf("first frame = %d %s, want 404 model_not_found", status, body)
	}

	// Rename the model row in place (same id, so the reconciler patches the live snapshot rather than reloading from the stores).
	renamed := *pinned.AllModels()[0]
	renamed.Meta.Name = "late-model"
	renamed.Spec.Snapshots = []model.Snapshot{{Name: slug.From("late-model")}}
	renamed.Spec.Pointer = slug.From("late-model")
	if err := applyModel(t, cat, &renamed); err != nil {
		t.Fatalf("upsert model: %v", err)
	}
	if cat.Current() == pinned {
		t.Fatal("catalog did not publish a new snapshot")
	}

	// Resolvable now, and refused for a different reason: the frame read the snapshot the reload published, not the one pinned at upgrade.
	if status, body := wsExchange(t, ctx, conn, "f2", payload); status != http.StatusBadRequest || errCode(t, []byte(body)) != "translate_request" {
		t.Fatalf("second frame = %d %s, want 400 translate_request", status, body)
	}
}

// A frame carries one request body, so a body the HTTP routes accept must fit in a frame too.
func TestWSFrameAcceptsBodiesUpToTheRequestLimit(t *testing.T) {
	cat, pr := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	payload := `{"model":"missing-model","messages":[],"pad":"` + strings.Repeat("x", 2<<20) + `"}`

	t.Run("within the limit", func(t *testing.T) {
		d := buildDeps(t, cat)
		d.MaxRequestBytes = 32 << 20
		conn, ctx := wsConn(t, d, pr)
		if status, body := wsExchange(t, ctx, conn, "big", payload); status != http.StatusNotFound || errCode(t, []byte(body)) != "model_not_found" {
			t.Fatalf("frame = %d %s, want it dispatched: 404 model_not_found", status, body)
		}
	})

	t.Run("over the limit", func(t *testing.T) {
		d := buildDeps(t, cat)
		d.MaxRequestBytes = 1 << 20
		conn, ctx := wsConn(t, d, pr)
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"id":"big","payload":`+payload+`}`)); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, _, err := conn.Read(ctx)
		var ce websocket.CloseError
		if !errors.As(err, &ce) || ce.Code != websocket.StatusMessageTooBig {
			t.Fatalf("read after an oversized frame: %v, want close %d", err, websocket.StatusMessageTooBig)
		}
	})
}

func TestWSReadLimit(t *testing.T) {
	// Unset falls back to the HTTP routes' own default.
	for maxBody, want := range map[int64]int64{
		1 << 20: 1<<20 + wsFrameEnvelopeBytes,
		0:       httpmw.DefaultMaxRequestBytes + wsFrameEnvelopeBytes,
		-1:      httpmw.DefaultMaxRequestBytes + wsFrameEnvelopeBytes,
	} {
		if got := wsReadLimit(maxBody); got != want {
			t.Errorf("wsReadLimit(%d) = %d, want %d", maxBody, got, want)
		}
	}
}
