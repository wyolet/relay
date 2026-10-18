package control

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/secret/keyring"
)

const (
	testKeyID  = "0192f000-0000-7000-8000-000000000001"
	testKeyID2 = "0192f000-0000-7000-8000-000000000002"
	testKEK    = "00112233445566778899aabbccddeeff"
)

// fakeSecretKeys records what the handlers asked for and answers with err when set.
type fakeSecretKeys struct {
	err      error
	rotated  []stored.DataKeyStatus
	rewrap   stored.RewrapResult
	scope    string
	all      bool
	to       string
	id       string
	force    bool
	destroys int
}

func newKey(id, scope, state string) stored.DataKeyStatus {
	return stored.DataKeyStatus{ID: id, Scope: scope, State: state, KEK: testKEK, CreatedAt: time.Unix(1700000000, 0).UTC()}
}

func (f *fakeSecretKeys) Status(context.Context) (stored.KeyStatus, error) {
	if f.err != nil {
		return stored.KeyStatus{}, f.err
	}
	k := newKey(testKeyID, "system", "primary")
	k.Rows = 3
	return stored.KeyStatus{DataKeys: []stored.DataKeyStatus{k}, TargetKEK: testKEK, HeldKEKs: []string{testKEK}}, nil
}

func (f *fakeSecretKeys) Rotate(_ context.Context, scope string) (stored.DataKeyStatus, error) {
	f.scope = scope
	if f.err != nil {
		return stored.DataKeyStatus{}, f.err
	}
	return newKey(testKeyID2, scope, "primary"), nil
}

func (f *fakeSecretKeys) RotateAll(context.Context) ([]stored.DataKeyStatus, error) {
	f.all = true
	return f.rotated, f.err
}

func (f *fakeSecretKeys) Rewrap(_ context.Context, to string) (stored.RewrapResult, error) {
	f.to = to
	return f.rewrap, f.err
}

func (f *fakeSecretKeys) Destroy(_ context.Context, id string, force bool) (stored.DataKeyStatus, error) {
	f.id, f.force = id, force
	f.destroys++
	if f.err != nil {
		return stored.DataKeyStatus{}, f.err
	}
	return newKey(id, "user:u1", "destroyed"), nil
}

type secretKeysHarness struct {
	h    http.Handler
	api  huma.API
	sink *auditSink
	em   *audit.Emitter
}

// newSecretKeysHarness mounts /secret-keys behind the audit middleware, RBAC and RequireActor, with the actor named by X-Test-Actor.
func newSecretKeysHarness(t *testing.T, keys SecretKeyManager) *secretKeysHarness {
	t.Helper()
	sink := &auditSink{}
	em := audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", scopeActors))
	r.Use(audit.Middleware(em, nil))
	api := humachi.New(r, huma.DefaultConfig("secret-keys-test", "0"))
	d := Deps{SecretKeys: keys, Authz: audit.Authorizer{Inner: testRBAC()}}
	registerSecretKeys(api, d, huma.Middlewares{httpapi.HumaAuth(RequireActor)})
	return &secretKeysHarness{h: r, api: api, sink: sink, em: em}
}

func (s *secretKeysHarness) events() []audit.Event {
	s.em.Close()
	return s.sink.all()
}

func TestSecretKeysStatus(t *testing.T) {
	s := newSecretKeysHarness(t, &fakeSecretKeys{})
	w := scopeReq(t, s.h, "root", http.MethodGet, "/secret-keys", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var body stored.KeyStatus
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.DataKeys) != 1 || body.DataKeys[0].Rows != 3 || body.TargetKEK != testKEK || len(body.HeldKEKs) != 1 {
		t.Fatalf("body = %+v", body)
	}
	if evs := s.events(); len(evs) != 0 {
		t.Fatalf("a read wrote audit rows: %+v", evs)
	}
}

func TestSecretKeysStatusWithoutAMasterKey(t *testing.T) {
	s := newSecretKeysHarness(t, &fakeSecretKeys{err: stored.ErrNoMasterKey})
	if w := scopeReq(t, s.h, "root", http.MethodGet, "/secret-keys", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body)
	}
}

func TestSecretKeysRotateScope(t *testing.T) {
	f := &fakeSecretKeys{}
	s := newSecretKeysHarness(t, f)
	w := scopeReq(t, s.h, "token", http.MethodPost, "/secret-keys/rotate", `{"scope":"project:p1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if f.scope != "project:p1" || f.all {
		t.Fatalf("rotated scope %q all=%v", f.scope, f.all)
	}
	evs := s.events()
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want one", evs)
	}
	ev := evs[0]
	if ev.Action != "secret-keys.rotate" || ev.Resource.Kind != "secret-key" || ev.Resource.ID != testKeyID2 ||
		ev.Resource.Name != "project:p1" || ev.Outcome.Status != audit.StatusAllowed || ev.Outcome.Code != http.StatusOK {
		t.Fatalf("event = %+v", ev)
	}
}

func TestSecretKeysRotateAll(t *testing.T) {
	f := &fakeSecretKeys{rotated: []stored.DataKeyStatus{newKey(testKeyID, "system", "primary"), newKey(testKeyID2, "user:u1", "primary")}}
	s := newSecretKeysHarness(t, f)
	w := scopeReq(t, s.h, "root", http.MethodPost, "/secret-keys/rotate", `{"all":true}`)
	if w.Code != http.StatusOK || !f.all {
		t.Fatalf("status = %d all=%v: %s", w.Code, f.all, w.Body)
	}
	evs := s.events()
	if len(evs) != 2 || evs[0].Resource.ID != testKeyID || evs[1].Resource.Name != "user:u1" {
		t.Fatalf("events = %+v, want one per rotated scope", evs)
	}
}

// The second of three scopes fails: the two keys that were made are audited as succeeded, beside the request's own error row.
func TestSecretKeysRotateAllPartialFailureAuditsRotatedKeys(t *testing.T) {
	f := &fakeSecretKeys{
		rotated: []stored.DataKeyStatus{newKey(testKeyID, "project:p1", "primary"), newKey(testKeyID2, "user:u1", "primary")},
		err:     fmt.Errorf("rotated 2 of 3 scopes: rotate scope %q: %w", "system", keyring.ErrConflict),
	}
	s := newSecretKeysHarness(t, f)
	w := scopeReq(t, s.h, "root", http.MethodPost, "/secret-keys/rotate", `{"all":true}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "rotated 2 of 3 scopes") {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	evs := s.events()
	if len(evs) != 3 {
		t.Fatalf("events = %+v, want two rotated keys and the request", evs)
	}
	for i, want := range []struct{ id, name string }{{testKeyID, "project:p1"}, {testKeyID2, "user:u1"}} {
		ev := evs[i]
		if ev.Action != "secret-keys.rotate" || ev.Resource.ID != want.id || ev.Resource.Name != want.name ||
			ev.Outcome.Status != audit.StatusAllowed || ev.Outcome.Code != http.StatusOK {
			t.Fatalf("event %d = %+v, want the rotated key %s", i, ev, want.id)
		}
	}
	req := evs[2]
	if req.Action != "secret-keys.rotate" || req.Resource.ID != "" || req.Resource.Name != "*" || req.Outcome.Code != http.StatusConflict {
		t.Fatalf("request row = %+v, want the failed rotate-all with 409", req)
	}
}

func TestSecretKeysRotateRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		err        error
		want       int
	}{
		{name: "neither scope nor all", body: `{}`, want: http.StatusBadRequest},
		{name: "both scope and all", body: `{"scope":"system","all":true}`, want: http.StatusBadRequest},
		{name: "unknown scope", body: `{"scope":"user:nobody"}`, err: fmt.Errorf("%w: %q", stored.ErrNoScopeKey, "user:nobody"), want: http.StatusNotFound},
		{name: "target KEK not held", body: `{"scope":"system"}`, err: fmt.Errorf("rotate: %w", keyring.ErrTargetKEKUnavailable), want: http.StatusConflict},
		{name: "concurrent rotation", body: `{"scope":"system"}`, err: keyring.ErrConflict, want: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSecretKeysHarness(t, &fakeSecretKeys{err: tc.err})
			if w := scopeReq(t, s.h, "root", http.MethodPost, "/secret-keys/rotate", tc.body); w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}
}

func TestSecretKeysRewrap(t *testing.T) {
	f := &fakeSecretKeys{rewrap: stored.RewrapResult{Target: testKEK, Rewrapped: 4, Skipped: []string{}}}
	s := newSecretKeysHarness(t, f)
	w := scopeReq(t, s.h, "root", http.MethodPost, "/secret-keys/rewrap", `{"to":"`+testKEK+`"}`)
	if w.Code != http.StatusOK || f.to != testKEK {
		t.Fatalf("status = %d to=%q: %s", w.Code, f.to, w.Body)
	}
	evs := s.events()
	if len(evs) != 1 || evs[0].Action != "secret-keys.rewrap" || evs[0].Resource.Name != testKEK || evs[0].Outcome.Code != http.StatusOK {
		t.Fatalf("events = %+v", evs)
	}
}

// Keys this pod cannot unwrap are named in the answer, not turned into a 500: the operator reruns where both KEKs are held.
func TestSecretKeysRewrapReportsSkippedKeys(t *testing.T) {
	f := &fakeSecretKeys{rewrap: stored.RewrapResult{Target: testKEK, Rewrapped: 1, Skipped: []string{testKeyID}}}
	s := newSecretKeysHarness(t, f)
	w := scopeReq(t, s.h, "root", http.MethodPost, "/secret-keys/rewrap", `{"to":"`+testKEK+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var body stored.RewrapResult
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Skipped) != 1 || body.Skipped[0] != testKeyID {
		t.Fatalf("body = %s (%v)", w.Body, err)
	}
}

func TestSecretKeysRewrapRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "KEK not held", err: fmt.Errorf("%w: not one of this process's master keys", keyring.ErrTargetKEKUnavailable), want: http.StatusConflict},
		{name: "legacy rows left", err: fmt.Errorf("%w: 2 rows", stored.ErrLegacyRows), want: http.StatusConflict},
		{name: "malformed KEK id", err: fmt.Errorf("%w: KEK id", stored.ErrBadKeyID), want: http.StatusBadRequest},
		{name: "no master key", err: stored.ErrNoMasterKey, want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSecretKeysHarness(t, &fakeSecretKeys{err: tc.err})
			w := scopeReq(t, s.h, "root", http.MethodPost, "/secret-keys/rewrap", `{"to":"`+testKEK+`"}`)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body)
			}
			if !strings.Contains(w.Body.String(), tc.err.Error()) {
				t.Fatalf("body %s does not carry the reason %q", w.Body, tc.err)
			}
			evs := s.events()
			if len(evs) != 1 || evs[0].Action != "secret-keys.rewrap" || evs[0].Outcome.Code != tc.want {
				t.Fatalf("refused rewrap not audited: %+v", evs)
			}
		})
	}
}

func TestSecretKeysDestroy(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		force      bool
	}{
		{name: "no body"},
		{name: "after the wait", body: `{}`},
		{name: "forced", body: `{"force":true}`, force: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSecretKeys{}
			s := newSecretKeysHarness(t, f)
			w := scopeReq(t, s.h, "root", http.MethodPost, "/secret-keys/by-id/"+testKeyID+"/destroy", tc.body)
			if w.Code != http.StatusOK || f.id != testKeyID || f.force != tc.force {
				t.Fatalf("status = %d id=%q force=%v: %s", w.Code, f.id, f.force, w.Body)
			}
			evs := s.events()
			if len(evs) != 1 || evs[0].Action != "secret-keys.destroy" || evs[0].Resource.ID != testKeyID || evs[0].Resource.Name != "user:u1" {
				t.Fatalf("events = %+v", evs)
			}
			forced := evs[0].Change != nil && strings.Contains(strings.Join(evs[0].Change.Fields, ","), "force")
			if forced != tc.force {
				t.Fatalf("change = %+v, want force recorded = %v", evs[0].Change, tc.force)
			}
		})
	}
}

func TestSecretKeysDestroyRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "primary", err: fmt.Errorf("data key %s: %w", testKeyID, keyring.ErrIsPrimary), want: http.StatusConflict},
		{name: "in use", err: fmt.Errorf("data key %s: %w", testKeyID, keyring.ErrInUse), want: http.StatusConflict},
		{name: "too recent", err: fmt.Errorf("data key %s: %w", testKeyID, keyring.ErrTooRecent), want: http.StatusConflict},
		{name: "unknown", err: fmt.Errorf("data key %s: %w", testKeyID, keyring.ErrUnknownKey), want: http.StatusNotFound},
		{name: "malformed id", err: fmt.Errorf("%w: data key id", stored.ErrBadKeyID), want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSecretKeysHarness(t, &fakeSecretKeys{err: tc.err})
			w := scopeReq(t, s.h, "root", http.MethodPost, "/secret-keys/by-id/"+testKeyID+"/destroy", `{}`)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body)
			}
			evs := s.events()
			if len(evs) != 1 || evs[0].Action != "secret-keys.destroy" || evs[0].Resource.ID != testKeyID || evs[0].Outcome.Code != tc.want {
				t.Fatalf("refused destroy not audited: %+v", evs)
			}
		})
	}
}

// Only an admin manages the key ring: a non-admin is refused before the manager is called, and the refusal is audited.
func TestSecretKeysNonAdminForbidden(t *testing.T) {
	for _, tc := range []struct{ method, path, body, action string }{
		{http.MethodGet, "/secret-keys", "", "secret-keys.list"},
		{http.MethodPost, "/secret-keys/rotate", `{"scope":"system"}`, "secret-keys.rotate"},
		{http.MethodPost, "/secret-keys/rewrap", `{"to":"` + testKEK + `"}`, "secret-keys.rewrap"},
		{http.MethodPost, "/secret-keys/by-id/" + testKeyID + "/destroy", `{"force":true}`, "secret-keys.destroy"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			f := &fakeSecretKeys{}
			s := newSecretKeysHarness(t, f)
			if w := scopeReq(t, s.h, "bob", tc.method, tc.path, tc.body); w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", w.Code, w.Body)
			}
			if f.scope != "" || f.all || f.to != "" || f.destroys != 0 {
				t.Fatalf("manager called for a non-admin: %+v", f)
			}
			evs := s.events()
			if len(evs) != 1 || evs[0].Action != tc.action || evs[0].Outcome.Status != audit.StatusDenied {
				t.Fatalf("events = %+v, want one denied %s", evs, tc.action)
			}
			if w := scopeReq(t, s.h, "", tc.method, tc.path, tc.body); w.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated status = %d, want 401", w.Code)
			}
		})
	}
}
