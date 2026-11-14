package control

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/pkg/meta"
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
	r, api := testAPI(t, scopeActors, audit.Middleware(em, nil))
	d := Deps{SecretKeys: keys, Authz: audit.Authorizer{Inner: testRBAC()}}
	registerSecretKeys(api, d, huma.Middlewares{httpapi.HumaAuth(RequireActor)})
	return &secretKeysHarness{h: r, api: api, sink: sink, em: em}
}

func (s *secretKeysHarness) events() []audit.Event {
	s.em.Close()
	return s.sink.all()
}

var rootActor = audit.Actor{Kind: audit.ActorUser, ID: "u-root", Name: "root"}

// keyEvent is the audit row of one /secret-keys write by root.
func keyEvent(action, path, id, name string, code int, fields ...string) audit.Event {
	ev := audit.Event{
		Actor: rootActor, Action: action, Resource: audit.Resource{Kind: "secret-key", ID: id, Name: name, Owner: &meta.Owner{Kind: meta.OwnerSystem}},
		Outcome: audit.Outcome{Status: audit.StatusAllowed, Code: code}, Request: audit.Request{Method: "POST", Path: path},
	}
	// The verdict stays allowed for a write the key ring refuses; a change is recorded only for one it made.
	if code < 400 {
		ev.Change = &audit.Change{Fields: fields}
	}
	return ev
}

func TestSecretKeysStatus(t *testing.T) {
	s := newSecretKeysHarness(t, &fakeSecretKeys{})
	w := scopeReq(t, s.h, "root", http.MethodGet, "/secret-keys", "")
	var body stored.KeyStatus
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK {
		t.Fatalf("status = %d (%v): %s", w.Code, err, w.Body)
	}
	primary := newKey(testKeyID, "system", "primary")
	primary.Rows = 3
	want := stored.KeyStatus{DataKeys: []stored.DataKeyStatus{primary}, TargetKEK: testKEK, HeldKEKs: []string{testKEK}}
	if diff := cmp.Diff(want, body); diff != "" {
		t.Fatalf("body (-want +got):\n%s", diff)
	}
	wantEvents(t, s.events())

	s = newSecretKeysHarness(t, &fakeSecretKeys{err: stored.ErrNoMasterKey})
	wantAPIError(t, scopeReq(t, s.h, "root", http.MethodGet, "/secret-keys", ""), 503, "", stored.ErrNoMasterKey.Error())
}

func TestSecretKeysRotate(t *testing.T) {
	const path = "/secret-keys/rotate"
	t.Run("one scope", func(t *testing.T) {
		f := &fakeSecretKeys{}
		s := newSecretKeysHarness(t, f)
		if w := scopeReq(t, s.h, "root", http.MethodPost, path, `{"scope":"project:p1"}`); w.Code != http.StatusOK || f.scope != "project:p1" || f.all {
			t.Fatalf("status = %d, rotated scope %q all=%v: %s", w.Code, f.scope, f.all, w.Body)
		}
		wantEvents(t, s.events(), keyEvent("secret-keys.rotate", path, testKeyID2, "project:p1", 200))
	})
	t.Run("every scope", func(t *testing.T) {
		f := &fakeSecretKeys{rotated: []stored.DataKeyStatus{newKey(testKeyID, "system", "primary"), newKey(testKeyID2, "user:u1", "primary")}}
		s := newSecretKeysHarness(t, f)
		if w := scopeReq(t, s.h, "root", http.MethodPost, path, `{"all":true}`); w.Code != http.StatusOK || !f.all || f.scope != "" {
			t.Fatalf("status = %d all=%v scope=%q: %s", w.Code, f.all, f.scope, w.Body)
		}
		wantEvents(t, s.events(), keyEvent("secret-keys.rotate", path, testKeyID, "system", 200), keyEvent("secret-keys.rotate", path, testKeyID2, "user:u1", 200))
	})
	// The second of three scopes fails: the two keys that were made are audited as succeeded, beside the request's own error row.
	t.Run("every scope, failing part way", func(t *testing.T) {
		err := fmt.Errorf("rotated 2 of 3 scopes: rotate scope %q: %w", "system", keyring.ErrConflict)
		f := &fakeSecretKeys{rotated: []stored.DataKeyStatus{newKey(testKeyID, "project:p1", "primary"), newKey(testKeyID2, "user:u1", "primary")}, err: err}
		s := newSecretKeysHarness(t, f)
		wantAPIError(t, scopeReq(t, s.h, "root", http.MethodPost, path, `{"all":true}`), 409, "", err.Error())
		failed := keyEvent("secret-keys.rotate", path, "", "*", 409)
		failed.Change = &audit.Change{}
		wantEvents(t, s.events(), keyEvent("secret-keys.rotate", path, testKeyID, "project:p1", 200), keyEvent("secret-keys.rotate", path, testKeyID2, "user:u1", 200), failed)
	})
	for _, tc := range []struct {
		name, body string
		err        error
		status     int
		msg, named string
	}{
		{"neither scope nor all", `{}`, nil, 400, "give either scope or all: true", ""},
		{"both scope and all", `{"scope":"system","all":true}`, nil, 400, "give either scope or all: true", "*"},
		{"unknown scope", `{"scope":"user:nobody"}`, fmt.Errorf("%w: %q", stored.ErrNoScopeKey, "user:nobody"), 404, "", "user:nobody"},
		{"target KEK not held", `{"scope":"system"}`, fmt.Errorf("rotate: %w", keyring.ErrTargetKEKUnavailable), 409, "", "system"},
		{"concurrent rotation", `{"scope":"system"}`, keyring.ErrConflict, 409, "", "system"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSecretKeysHarness(t, &fakeSecretKeys{err: tc.err})
			msg := tc.msg
			if tc.err != nil {
				msg = tc.err.Error()
			}
			wantAPIError(t, scopeReq(t, s.h, "root", http.MethodPost, path, tc.body), tc.status, "", msg)
			wantEvents(t, s.events(), keyEvent("secret-keys.rotate", path, "", tc.named, tc.status))
		})
	}
}

func TestSecretKeysRewrap(t *testing.T) {
	const path = "/secret-keys/rewrap"
	body := `{"to":"` + testKEK + `"}`
	// Keys this pod cannot unwrap are named in the answer, not turned into a 500: the operator reruns where both KEKs are held.
	done := stored.RewrapResult{Target: testKEK, Rewrapped: 4, Skipped: []string{testKeyID}}
	f := &fakeSecretKeys{rewrap: done}
	s := newSecretKeysHarness(t, f)
	w := scopeReq(t, s.h, "root", http.MethodPost, path, body)
	var got stored.RewrapResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || f.to != testKEK {
		t.Fatalf("status = %d (%v) to=%q: %s", w.Code, err, f.to, w.Body)
	}
	if diff := cmp.Diff(done, got); diff != "" {
		t.Fatalf("body (-want +got):\n%s", diff)
	}
	// A rewrap changes how keys are wrapped, not a field, and records no change.
	recorded := keyEvent("secret-keys.rewrap", path, "", testKEK, 200)
	recorded.Change = nil
	wantEvents(t, s.events(), recorded)

	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"KEK not held", fmt.Errorf("%w: not one of this process's master keys", keyring.ErrTargetKEKUnavailable), 409},
		{"legacy rows left", fmt.Errorf("%w: 2 rows", stored.ErrLegacyRows), 409},
		{"malformed KEK id", fmt.Errorf("%w: KEK id", stored.ErrBadKeyID), 400},
		{"no master key", stored.ErrNoMasterKey, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSecretKeysHarness(t, &fakeSecretKeys{err: tc.err})
			wantAPIError(t, scopeReq(t, s.h, "root", http.MethodPost, path, body), tc.want, "", tc.err.Error())
			wantEvents(t, s.events(), keyEvent("secret-keys.rewrap", path, "", testKEK, tc.want))
		})
	}
}

func TestSecretKeysDestroy(t *testing.T) {
	const path = "/secret-keys/by-id/" + testKeyID + "/destroy"
	for _, tc := range []struct {
		name, body string
		force      bool
	}{
		{"no body", "", false},
		{"after the wait", `{}`, false},
		{"forced", `{"force":true}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSecretKeys{}
			s := newSecretKeysHarness(t, f)
			if w := scopeReq(t, s.h, "root", http.MethodPost, path, tc.body); w.Code != http.StatusOK || f.id != testKeyID || f.force != tc.force {
				t.Fatalf("status = %d id=%q force=%v: %s", w.Code, f.id, f.force, w.Body)
			}
			want := keyEvent("secret-keys.destroy", path, testKeyID, "user:u1", 200, "state")
			if tc.force {
				want.Change.Fields = []string{"state", "force"}
			}
			wantEvents(t, s.events(), want)
		})
	}
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"primary", fmt.Errorf("data key %s: %w", testKeyID, keyring.ErrIsPrimary), 409},
		{"in use", fmt.Errorf("data key %s: %w", testKeyID, keyring.ErrInUse), 409},
		{"too recent", fmt.Errorf("data key %s: %w", testKeyID, keyring.ErrTooRecent), 409},
		{"unknown", fmt.Errorf("data key %s: %w", testKeyID, keyring.ErrUnknownKey), 404},
		{"malformed id", fmt.Errorf("%w: data key id", stored.ErrBadKeyID), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSecretKeysHarness(t, &fakeSecretKeys{err: tc.err})
			wantAPIError(t, scopeReq(t, s.h, "root", http.MethodPost, path, `{}`), tc.want, "", tc.err.Error())
			wantEvents(t, s.events(), keyEvent("secret-keys.destroy", path, testKeyID, "", tc.want))
		})
	}
}

// Only an admin manages the key ring: anyone else is refused before the manager is called, and the refusal is audited.
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
			wantAPIError(t, scopeReq(t, s.h, "bob", tc.method, tc.path, tc.body), 403, "", "forbidden")
			if w := scopeReq(t, s.h, "", tc.method, tc.path, tc.body); w.Code != http.StatusUnauthorized || w.Body.String() != unauthenticatedBody {
				t.Fatalf("no actor: %d %s, want 401", w.Code, w.Body)
			}
			if diff := cmp.Diff(&fakeSecretKeys{}, f, cmp.AllowUnexported(fakeSecretKeys{})); diff != "" {
				t.Fatalf("the manager was called (-untouched +got):\n%s", diff)
			}
			evs := s.events()
			if len(evs) != 1 || evs[0].Action != tc.action || evs[0].Outcome != (audit.Outcome{Status: audit.StatusDenied, Code: 403}) || evs[0].Actor.ID != "u-bob" {
				t.Fatalf("events = %+v, want bob's one denied %s", evs, tc.action)
			}
		})
	}
}
