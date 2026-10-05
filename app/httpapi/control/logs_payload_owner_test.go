package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/payload"
	payloadfile "github.com/wyolet/relay/pkg/payload/file"
	"github.com/wyolet/relay/pkg/reqid"
)

// eventsByID answers a request_id lookup with that id's events, newest first.
type eventsByID struct {
	fakeUsageReader
	byID map[string][]usagelog.Event
}

func (f *eventsByID) Events(_ context.Context, q usagelog.EventQuery) ([]usagelog.Event, error) {
	return f.byID[q.RequestID], nil
}

type logGetBody struct {
	Log     usagelog.Event `json:"log"`
	Payload *struct {
		RequestBody string `json:"request_body"`
	} `json:"payload"`
}

func newLogsHarness(t *testing.T, ur usagelog.Reader, pr payload.Reader) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if a, ok := scopeActors[req.Header.Get("X-Test-Actor")]; ok {
				req = req.WithContext(actor.WithActor(req.Context(), a))
			}
			next.ServeHTTP(w, req)
		})
	})
	api := humachi.New(r, huma.DefaultConfig("logs-owner-test", "0"))
	registerLogs(api, Deps{Authz: testRBAC(), UsageReader: ur, PayloadReader: pr}, nil)
	return r
}

func getLog(t *testing.T, h http.Handler, who, id string) (int, logGetBody) {
	t.Helper()
	w := scopeReq(t, h, who, http.MethodGet, "/logs/"+id, "")
	var out logGetBody
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, out
}

// Two principals send the same caller-chosen X-Request-ID: each request is
// stored under its own relay-minted id, so neither record replaces the other
// and each principal reads back only its own body.
func TestLogsSameClientRequestIDStaysPerPrincipal(t *testing.T) {
	const clientID = "client-req-0001"
	mint := func() string {
		var id string
		h := reqid.Middleware(slog.Default())(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			id = reqid.From(r.Context())
		}))
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set(reqid.HeaderInbound, clientID)
		h.ServeHTTP(httptest.NewRecorder(), req)
		return id
	}
	bobID, aliceID := mint(), mint()
	if bobID == clientID || aliceID == clientID || bobID == aliceID {
		t.Fatalf("request ids bob=%q alice=%q: want distinct relay-minted ids", bobID, aliceID)
	}

	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	sink, err := payloadfile.NewSink(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, r := range []payload.Record{
		{RequestID: bobID, Timestamp: now.Add(-time.Hour), PrincipalID: "u-bob", RequestBody: []byte("BOB-PROMPT")},
		{RequestID: aliceID, Timestamp: now, PrincipalID: "u-alice", RequestBody: []byte("ALICE-PROMPT")},
	} {
		if err := sink.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	_ = sink.Close()

	ur := &eventsByID{byID: map[string][]usagelog.Event{
		bobID:   {{RequestID: bobID, PrincipalID: "u-bob", Timestamp: now.Add(-time.Hour)}},
		aliceID: {{RequestID: aliceID, PrincipalID: "u-alice", Timestamp: now}},
	}}
	h := newLogsHarness(t, ur, payloadfile.NewReader(path))

	for _, tc := range []struct {
		who, id, body string
	}{
		{"bob", bobID, "BOB-PROMPT"},
		{"alice", aliceID, "ALICE-PROMPT"},
	} {
		code, out := getLog(t, h, tc.who, tc.id)
		if code != http.StatusOK || out.Payload == nil || out.Payload.RequestBody != tc.body {
			t.Fatalf("%s reading own %s: code=%d payload=%+v, want %q", tc.who, tc.id, code, out.Payload, tc.body)
		}
	}
	if code, _ := getLog(t, h, "alice", bobID); code != http.StatusNotFound {
		t.Fatalf("alice reading bob's id: code=%d, want 404", code)
	}
	if code, _ := getLog(t, h, "bob", aliceID); code != http.StatusNotFound {
		t.Fatalf("bob reading alice's id: code=%d, want 404", code)
	}
}

// A stored body is attached only when it belongs to the same owner as the
// log record that passed the scope check — a shared id (records written
// before ids were relay-minted) must not join another principal's body.
func TestLogsPayloadJoinRequiresSameOwner(t *testing.T) {
	const sharedID = "client-req-0001"
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	sink, err := payloadfile.NewSink(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(payload.Record{
		RequestID: sharedID, Timestamp: time.Now().Add(-time.Hour), PrincipalID: "u-bob",
		RequestBody: []byte("VICTIM-PROMPT"), ResponseBody: []byte("VICTIM-COMPLETION"),
	}); err != nil {
		t.Fatal(err)
	}
	_ = sink.Close()

	ur := &eventsByID{byID: map[string][]usagelog.Event{
		sharedID: {{RequestID: sharedID, PrincipalID: "u-alice", Timestamp: time.Now()}},
	}}
	h := newLogsHarness(t, ur, payloadfile.NewReader(path))

	code, out := getLog(t, h, "alice", sharedID)
	if code != http.StatusOK {
		t.Fatalf("code=%d, want 200 for alice's own log record", code)
	}
	if out.Log.PrincipalID != "u-alice" {
		t.Fatalf("log principal = %q, want u-alice", out.Log.PrincipalID)
	}
	if out.Payload != nil {
		t.Fatalf("payload = %+v, want none: the stored body belongs to another principal", out.Payload)
	}
}

// A body stored without owner fields (captured before they were recorded)
// joins only for a reader who may already read every record.
func TestLogsPayloadWithoutOwnerOnlyForUnrestrictedReader(t *testing.T) {
	const id = "legacy-0001"
	path := filepath.Join(t.TempDir(), "payloads.jsonl")
	sink, err := payloadfile.NewSink(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(payload.Record{RequestID: id, Timestamp: time.Now(), RequestBody: []byte("OLD")}); err != nil {
		t.Fatal(err)
	}
	_ = sink.Close()
	ur := &eventsByID{byID: map[string][]usagelog.Event{
		id: {{RequestID: id, PrincipalID: "u-alice", Timestamp: time.Now()}},
	}}
	h := newLogsHarness(t, ur, payloadfile.NewReader(path))

	if code, out := getLog(t, h, "alice", id); code != http.StatusOK || out.Payload != nil {
		t.Fatalf("scoped reader: code=%d payload=%+v, want 200 without payload", code, out.Payload)
	}
	if code, out := getLog(t, h, "root", id); code != http.StatusOK || out.Payload == nil || out.Payload.RequestBody != "OLD" {
		t.Fatalf("unrestricted reader: code=%d payload=%+v, want the stored body", code, out.Payload)
	}
}
