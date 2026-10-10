package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/payloadlog"
	payloadfile "github.com/wyolet/relay/pkg/payload/file"
)

type fakeEraser struct {
	res    payloadlog.EraseResult
	err    error
	called []payloadlog.EraseFilter
}

func (f *fakeEraser) Get(context.Context, string) (payloadlog.Record, error) {
	return payloadlog.Record{}, payloadlog.ErrNotFound
}

func (f *fakeEraser) Erase(_ context.Context, ef payloadlog.EraseFilter) (payloadlog.EraseResult, error) {
	f.called = append(f.called, ef)
	return f.res, f.err
}

func newEraseHarness(t *testing.T, inner authz.Authorizer, pr payloadlog.Reader) (http.Handler, *auditSink, *audit.Emitter) {
	t.Helper()
	sink := &auditSink{}
	em := audit.NewEmitter(sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", scopeActors))
	r.Use(audit.Middleware(em, nil))
	api := humachi.New(r, huma.DefaultConfig("logs-erase-test", "0"))
	registerLogsErase(api, Deps{Authz: audit.Authorizer{Inner: inner}, PayloadReader: pr}, nil)
	return r, sink, em
}

func onlyEvent(t *testing.T, sink *auditSink, em *audit.Emitter) audit.Event {
	t.Helper()
	em.Close()
	evs := sink.all()
	if len(evs) != 1 {
		t.Fatalf("audit events = %d, want 1: %+v", len(evs), evs)
	}
	return evs[0]
}

func TestLogsEraseRequiresAdmin(t *testing.T) {
	for name, inner := range map[string]authz.Authorizer{
		"single": authz.AlwaysAllowAuthenticated{},
		"rbac":   testRBAC(),
	} {
		t.Run(name, func(t *testing.T) {
			eraser := &fakeEraser{}
			h, sink, em := newEraseHarness(t, inner, eraser)
			w := scopeReq(t, h, "alice", http.MethodPost, "/logs/erase", `{"projectId":"p-1"}`)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
			}
			if len(eraser.called) != 0 {
				t.Fatal("a non-admin reached the eraser")
			}
			ev := onlyEvent(t, sink, em)
			if ev.Action != "logs.erase" || ev.Outcome.Status != audit.StatusDenied || ev.Outcome.Code != http.StatusForbidden {
				t.Fatalf("event = action %q outcome %+v, want logs.erase denied/403", ev.Action, ev.Outcome)
			}
		})
	}
}

func TestLogsEraseRequiresFilter(t *testing.T) {
	eraser := &fakeEraser{}
	h, _, em := newEraseHarness(t, testRBAC(), eraser)
	defer em.Close()
	for _, body := range []string{`{}`, `{"projectId":"","principalId":""}`} {
		if w := scopeReq(t, h, "root", http.MethodPost, "/logs/erase", body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400: %s", body, w.Code, w.Body.String())
		}
	}
	if len(eraser.called) != 0 {
		t.Fatal("an empty filter reached the eraser")
	}
}

func TestLogsEraseUnsupportedBackend(t *testing.T) {
	for name, pr := range map[string]payloadlog.Reader{
		"reader without Erase": payloadfile.NewReader(filepath.Join(t.TempDir(), "payloads.jsonl")),
		"backend refuses":      &fakeEraser{err: fmt.Errorf("%w (file)", payloadlog.ErrEraseUnsupported)},
	} {
		t.Run(name, func(t *testing.T) {
			h, _, em := newEraseHarness(t, testRBAC(), pr)
			defer em.Close()
			if w := scopeReq(t, h, "root", http.MethodPost, "/logs/erase", `{"principalId":"u-1"}`); w.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestLogsEraseReturnsCountAndAudits(t *testing.T) {
	eraser := &fakeEraser{res: payloadlog.EraseResult{Requests: 3}}
	h, sink, em := newEraseHarness(t, testRBAC(), eraser)
	w := scopeReq(t, h, "token", http.MethodPost, "/logs/erase", `{"projectId":"p-1","principalId":"u-1"}`)
	var out struct {
		Requests uint64 `json:"requests"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Requests != 3 {
		t.Fatalf("status = %d body = %s, want 200 with requests 3", w.Code, w.Body.String())
	}
	if want := (payloadlog.EraseFilter{ProjectID: "p-1", PrincipalID: "u-1"}); len(eraser.called) != 1 || eraser.called[0] != want {
		t.Fatalf("eraser called with %+v, want %+v", eraser.called, want)
	}
	ev := onlyEvent(t, sink, em)
	if ev.Action != "logs.erase" || ev.Outcome.Status != audit.StatusAllowed || ev.Outcome.Code != http.StatusOK {
		t.Fatalf("event = action %q outcome %+v, want logs.erase allowed/200", ev.Action, ev.Outcome)
	}
	if ev.Resource.Kind != "logs" || ev.Resource.ID != "u-1" ||
		ev.Resource.Owner == nil || *ev.Resource.Owner != (meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}) {
		t.Fatalf("resource = %+v, want the filter's principal and project", ev.Resource)
	}
}
