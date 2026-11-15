package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/pkg/meta"
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
	em := audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
	r, api := testAPI(t, scopeActors, audit.Middleware(em, nil))
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

// A refusal before the eraser runs leaves it uncalled; a failure inside it is recorded, since some bodies may already be gone.
func TestLogsEraseRefusals(t *testing.T) {
	noErase := payloadfile.NewReader(filepath.Join(t.TempDir(), "payloads.jsonl"))
	for _, tc := range []struct {
		name, body string
		pr         payloadlog.Reader
		status     int
		code, msg  string
		audited    string
	}{
		{"no filter", `{}`, &fakeEraser{}, http.StatusBadRequest, "", "projectId or principalId is required", ""},
		{"empty filter", `{"projectId":"","principalId":""}`, &fakeEraser{}, http.StatusBadRequest, "", "projectId or principalId is required", ""},
		{"reader without Erase", `{"principalId":"u-1"}`, noErase, http.StatusNotImplemented, "", "the payload backend cannot erase captured bodies", ""},
		{"backend refuses", `{"principalId":"u-1"}`, &fakeEraser{err: fmt.Errorf("%w (file)", payloadlog.ErrEraseUnsupported)}, http.StatusNotImplemented, "", payloadlog.ErrEraseUnsupported.Error() + " (file)", ""},
		{"backend fails", `{"principalId":"u-1"}`, &fakeEraser{err: errors.New("disk full")}, http.StatusInternalServerError, "internal_error", "disk full", audit.StatusError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, sink, em := newEraseHarness(t, testRBAC(), tc.pr)
			wantAPIError(t, scopeReq(t, h, "root", http.MethodPost, "/logs/erase", tc.body), tc.status, tc.code, tc.msg)
			if eraser, ok := tc.pr.(*fakeEraser); ok && eraser.err == nil && len(eraser.called) != 0 {
				t.Fatal("a request with no filter reached the eraser")
			}
			em.Close()
			evs := sink.all()
			if tc.audited == "" && len(evs) != 0 {
				t.Fatalf("audit events = %+v, want none", evs)
			}
			if tc.audited != "" && (len(evs) != 1 || evs[0].Action != "logs.erase" || evs[0].Outcome != (audit.Outcome{Status: tc.audited, Code: tc.status})) {
				t.Fatalf("audit events = %+v, want one logs.erase %s/%d", evs, tc.audited, tc.status)
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
