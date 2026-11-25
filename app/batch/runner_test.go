package batch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/revocation"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/adapterspecs"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/meta"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/pkg/usage"
	pkgrelay "github.com/wyolet/relay/sdk/v1"
)

const itemBody = `{"model":"test-model"}`

// sent is one request an upstream received.
type sent struct{ Path, Auth, Body string }

// upstream answers each request with respond's status and body, recording what it was sent.
type upstream struct {
	*httptest.Server
	mu    sync.Mutex
	calls []sent
}

func newUpstream(t *testing.T, respond func(n int) (int, string)) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.calls = append(u.calls, sent{r.URL.Path, r.Header.Get("Authorization"), string(body)})
		n := len(u.calls)
		u.mu.Unlock()
		status, out := respond(n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(u.Close)
	return u
}

func answering(status int, body string) func(int) (int, string) {
	return func(int) (int, string) { return status, body }
}

func (u *upstream) sent() []sent {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]sent(nil), u.calls...)
}

// usageCapture is a lifecycle.Collector handing each finalized item's usage event to the test.
type usageCapture chan usagelog.Event

func (c usageCapture) Collect(lc *lifecycle.Context) {
	if v, ok := lc.Collected(usagelog.Namespace); ok {
		c <- *v.(*usagelog.Event)
	}
}

func (c usageCapture) next(t *testing.T) usagelog.Event {
	t.Helper()
	select {
	case ev := <-c:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no usage event emitted")
		return usagelog.Event{}
	}
}

// fixture is a Runner over published plans: one model served at one host through a system-owned credential, one user, that user's key, and a project of a team the user's tokens act in.
type fixture struct {
	rn      *Runner
	up      *upstream
	events  usageCapture
	plans   *plantest.Plans
	revoked *revocation.List
	userID  string
	key     *key.Key
	team    *team.Team
	project *project.Project
}

// keyAttr and tokenAttr are submissions by the fixture's key and by a token of its user in the fixture's project.
func (f *fixture) keyAttr() Attribution {
	return Attribution{PrincipalKind: string(key.PrincipalUser), PrincipalID: f.userID, CredentialKind: inference.CredentialKey, CredentialID: f.key.Meta.ID}
}

func (f *fixture) tokenAttr(jti string) Attribution {
	a := Attribution{PrincipalKind: string(key.PrincipalUser), PrincipalID: f.userID, CredentialKind: inference.CredentialToken, CredentialID: jti}
	a.ProjectID, a.TeamID = f.project.Meta.ID, f.team.Meta.ID
	return a
}

func (f *fixture) runKey(attr Attribution, body string) (int, []byte, error) {
	return f.rn.Run(context.Background(), "item-1", f.key.Spec.KeyHash, TokenClaims{}, attr, adapters.OpenAI, []byte(body))
}

func (f *fixture) runToken(tok TokenClaims) (int, []byte, error) {
	return f.rn.Run(context.Background(), "item-1", "", tok, f.tokenAttr(tok.JTI), adapters.OpenAI, []byte(itemBody))
}

// runnerFixture wires a Runner whose pipeline counts on kv.Mem, against an upstream answering as respond says.
func runnerFixture(t *testing.T, respond func(int) (int, string)) *fixture {
	t.Helper()
	up := newUpstream(t, respond)
	var w plantest.World
	prov := w.Provider("vendor")
	h := w.Host("vendor", up.URL)
	m := w.Model(prov, "test-model")
	w.Bind(m, h, adapters.OpenAI)
	w.Credential(h, nil, plantest.System, "sk-test")
	u := w.User()
	k := w.UserKey(u, "rk-secret")
	k.Meta.Name = "rk"
	tm := w.Team()
	proj := w.Project(tm)
	w.Member(u, meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID})
	plans := w.Load(t)
	rn, revoked, events := runnerOver(t, plans)
	return &fixture{rn: rn, up: up, events: events, plans: plans, revoked: revoked, userID: u.ID, key: k, team: tm, project: proj}
}

// runnerOver wires a Runner over published plans, with a pipeline counting on kv.Mem, the revoked-token list beside it and its usage events captured.
func runnerOver(t *testing.T, plans *plantest.Plans) (*Runner, *revocation.List, usageCapture) {
	t.Helper()
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	pkgratelimit.RegisterScripts(mem)
	revoked := revocation.New(mem)
	events := make(usageCapture, 4)
	reg := lifecycle.New()
	reg.RegisterHook(usagelog.NewUsageHook(nil, ""))
	reg.RegisterCollector(events)

	spec := func(name adapters.Name) *adapter.Spec {
		return (&adapter.Spec{
			Name:        name,
			DefaultPath: "/v1/chat/completions",
			Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
			Translator:  pkgrelay.IdentityTranslator{},
		}).Build()
	}
	return &Runner{
		Auth: &inference.Auth{Plans: plans.Holder, Revoked: revoked.Set()},
		Pipeline: &pipeline.Pipeline{
			Limiter:   lua.New(pkgratelimit.New(mem, slog.Default(), nil)),
			Selector:  keypool.New(mem, slog.Default(), nil, nil),
			Logger:    slog.Default(),
			Lifecycle: reg,
		},
		Specs: adapter.NewRegistry(spec(adapters.OpenAI), spec(adapters.Anthropic)),
	}, revoked, events
}

// attribution is the part of a usage event a batch item fixes.
type attribution struct {
	RequestID, Source            string
	Status                       int
	ErrorKind                    string
	RelayKeyHash, RelayKey       string
	RequestedModel, Model        string
	ProjectID, Project           string
	TeamID, Team                 string
	PrincipalKind, PrincipalID   string
	CredentialKind, CredentialID string
	LaneAccountID                string
}

func attributionOf(ev usagelog.Event) attribution {
	return attribution{
		ev.RequestID, ev.Source, ev.Status, ev.ErrorKind, ev.RelayKeyHash, ev.RelayKey, ev.RequestedModel, ev.Model,
		ev.ProjectID, ev.Project, ev.TeamID, ev.Team, ev.PrincipalKind, ev.PrincipalID, ev.CredentialKind, ev.CredentialID, ev.LaneAccountID,
	}
}

// An item goes upstream once under the route's credential and its answer comes back verbatim; its usage event carries the submission's attribution and the lane it ran in.
func TestRun_SendsTheItemAndAttributesItsUsage(t *testing.T) {
	const answer = `{"id":"x","usage":{"prompt_tokens":1,"completion_tokens":1}}`
	f := runnerFixture(t, answering(200, answer))
	member, refused := f.plans.Root().MemberLane(f.userID, f.project.Meta.ID)
	if refused != nil {
		t.Fatal(refused)
	}
	switched := f.keyAttr()
	switched.LaneID = member.Lane.Meta.ID
	keyEvent := attribution{
		RequestID: "item-1", Source: "batch", Status: 200, RelayKeyHash: f.key.Spec.KeyHash, RelayKey: "rk",
		RequestedModel: "test-model", Model: "test-model", PrincipalKind: "user", PrincipalID: f.userID,
		CredentialKind: inference.CredentialKey, CredentialID: f.key.Meta.ID, LaneAccountID: f.key.Spec.AccountID,
	}
	switchedEvent := keyEvent
	switchedEvent.LaneAccountID = member.Lane.Meta.ID
	tokenEvent := attribution{
		RequestID: "item-1", Source: "batch", Status: 200, RequestedModel: "test-model", Model: "test-model",
		ProjectID: f.project.Meta.ID, Project: f.project.Meta.Name, TeamID: f.team.Meta.ID, Team: f.team.Meta.Name,
		PrincipalKind: "user", PrincipalID: f.userID, CredentialKind: inference.CredentialToken, CredentialID: "jti-1",
		LaneAccountID: member.Lane.Meta.ID,
	}

	for i, tc := range []struct {
		name string
		run  func() (int, []byte, error)
		want attribution
	}{
		{"key in its own lane", func() (int, []byte, error) { return f.runKey(f.keyAttr(), itemBody) }, keyEvent},
		{"key switched into a member lane", func() (int, []byte, error) { return f.runKey(switched, itemBody) }, switchedEvent},
		{"token in its project", func() (int, []byte, error) { return f.runToken(TokenClaims{JTI: "jti-1"}) }, tokenEvent},
	} {
		status, out, err := tc.run()
		if err != nil || status != 200 || string(out) != answer {
			t.Fatalf("%s: Run = %d, %q, %v; want 200 and the upstream's answer", tc.name, status, out, err)
		}
		wantSent := sent{"/v1/chat/completions", "Bearer sk-test", itemBody}
		if calls := f.up.sent(); len(calls) != i+1 || calls[i] != wantSent {
			t.Fatalf("%s: upstream saw %+v, want call %d to be %+v", tc.name, calls, i+1, wantSent)
		}
		if d := cmp.Diff(tc.want, attributionOf(f.events.next(t))); d != "" {
			t.Errorf("%s: usage event (-want +got):\n%s", tc.name, d)
		}
	}
}

// A 400 is the item's result; a 500 fails the item once its only credential is spent. Both are recorded as the upstream's error.
func TestRun_UpstreamErrorStatus(t *testing.T) {
	const rejected = `{"error":{"message":"rejected"}}`
	for _, tc := range []struct {
		status  int
		wantErr error
		calls   int
	}{
		{http.StatusBadRequest, nil, 1},
		{http.StatusInternalServerError, pipeline.ErrAllKeysExhausted, 1},
	} {
		f := runnerFixture(t, answering(tc.status, rejected))
		status, out, err := f.runKey(f.keyAttr(), itemBody)
		if !errors.Is(err, tc.wantErr) {
			t.Fatalf("upstream %d: err = %v, want %v", tc.status, err, tc.wantErr)
		}
		if tc.wantErr == nil && (status != tc.status || string(out) != rejected) {
			t.Errorf("upstream %d: Run = %d, %q; want the upstream's status and body", tc.status, status, out)
		}
		if tc.wantErr != nil && (status != 0 || out != nil) {
			t.Errorf("upstream %d: Run = %d, %q beside its error; want 0, nil", tc.status, status, out)
		}
		if n := len(f.up.sent()); n != tc.calls {
			t.Errorf("upstream %d: %d upstream calls, want %d", tc.status, n, tc.calls)
		}
		ev := f.events.next(t)
		if ev.Source != "batch" || ev.Status != tc.status || ev.ErrorKind != usage.ErrorKindUpstream {
			t.Errorf("upstream %d: event source=%q status=%d kind=%q", tc.status, ev.Source, ev.Status, ev.ErrorKind)
		}
	}
}

// A queued item runs under the credential that submitted it, so that credential has to still be valid when the item runs; a refused item never reaches the upstream.
func TestRun_RefusesLapsedCredential(t *testing.T) {
	f := runnerFixture(t, answering(200, `{}`))
	if err := f.revoked.Revoke(context.Background(), "jti-revoked", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	for _, tc := range []struct {
		name string
		hash string
		tok  TokenClaims
		want string
	}{
		{"key no longer published", "gone-key-hash", TokenClaims{}, "api key no longer valid"},
		{"token revoked after submit", "", TokenClaims{JTI: "jti-revoked"}, "token revoked"},
		{"token from before revoke-all", "", TokenClaims{JTI: "j", Version: 7}, "token revoked"},
		{"token past its expiry", "", TokenClaims{JTI: "j", Expires: time.Now().Add(-time.Minute).Unix()}, "token expired"},
		{"token expiring this second", "", TokenClaims{JTI: "j", Expires: time.Now().Unix()}, "token expired"},
	} {
		attr := f.tokenAttr(tc.tok.JTI)
		if tc.hash != "" {
			attr = f.keyAttr()
		}
		status, out, err := f.rn.Run(context.Background(), "item", tc.hash, tc.tok, attr, adapters.OpenAI, []byte(itemBody))
		want := "batch: credential no longer valid: " + tc.want
		if !errors.Is(err, ErrCredentialInvalid) || err.Error() != want || status != 0 || out != nil {
			t.Errorf("%s: Run = %d, %q, %v; want %q", tc.name, status, out, err, want)
		}
	}
	if calls := f.up.sent(); len(calls) != 0 {
		t.Fatalf("refused items reached the upstream: %+v", calls)
	}

	for name, run := range map[string]func() (int, []byte, error){
		"live key":   func() (int, []byte, error) { return f.runKey(f.keyAttr(), itemBody) },
		"live token": func() (int, []byte, error) { return f.runToken(TokenClaims{JTI: "jti-live"}) },
		"token not yet due": func() (int, []byte, error) {
			return f.runToken(TokenClaims{JTI: "j", Expires: time.Now().Add(time.Minute).Unix()})
		},
	} {
		if status, _, err := run(); err != nil || status != 200 {
			t.Errorf("%s: Run = %d, %v; want 200", name, status, err)
		}
	}
}

// A token acts in its project, so its queued items stop when the project or its team is disabled or gone, as its live requests do.
func TestRun_RefusesTokenItemsWhoseProjectLapsed(t *testing.T) {
	off := false
	for name, lapse := range map[string]func(f *fixture){
		"project disabled": func(f *fixture) {
			p := *f.project
			p.Spec.Enabled = &off
			plantest.Apply(t, f.plans, &p)
		},
		"team disabled": func(f *fixture) {
			tm := *f.team
			tm.Spec.Enabled = &off
			plantest.Apply(t, f.plans, &tm)
		},
		"project gone": func(f *fixture) {
			if err := engine.Delete(f.plans.Engine, engine.Ref{Kind: plan.KindProject, ID: f.project.Meta.ID}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		f := runnerFixture(t, answering(200, `{}`))
		if status, _, err := f.runToken(TokenClaims{JTI: "j"}); err != nil || status != 200 {
			t.Fatalf("%s: before the lapse Run = %d, %v; want 200", name, status, err)
		}
		lapse(f)
		if _, _, err := f.runToken(TokenClaims{JTI: "j"}); !errors.Is(err, ErrCredentialInvalid) {
			t.Errorf("%s: err = %v, want ErrCredentialInvalid", name, err)
		}
		if n := len(f.up.sent()); n != 1 {
			t.Errorf("%s: %d upstream calls, want only the one before the lapse", name, n)
		}
	}
}

// An item that cannot be dispatched as submitted fails without an upstream call.
func TestRun_RefusesUndispatchableItems(t *testing.T) {
	f := runnerFixture(t, answering(200, `{}`))
	for _, tc := range []struct {
		name    string
		inbound adapters.Name
		body    string
		is      error
		want    string // prefix: the JSON case ends in the decoder's own text
	}{
		{"cross-shape", adapters.Anthropic, itemBody, ErrCrossShape, "batch: cross-shape dispatch not yet supported (anthropic→openai)"},
		{"inbound shape not registered", "nope", itemBody, nil, "batch: adapter not registered for nope/openai"},
		{"body is not JSON", adapters.OpenAI, `not json`, nil, "batch: parse model: invalid JSON: "},
		{"body has no model", adapters.OpenAI, `{}`, nil, `batch: route "": model_not_found: no model named `},
		{"unknown model", adapters.OpenAI, `{"model":"nope"}`, nil, `batch: route "nope": model_not_found: no model named nope`},
	} {
		status, out, err := f.rn.Run(context.Background(), "item", f.key.Spec.KeyHash, TokenClaims{}, f.keyAttr(), tc.inbound, []byte(tc.body))
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) || (tc.is != nil && !errors.Is(err, tc.is)) || status != 0 || out != nil {
			t.Errorf("%s: Run = %d, %q, %v; want %q", tc.name, status, out, err, tc.want)
		}
	}
	if calls := f.up.sent(); len(calls) != 0 {
		t.Errorf("undispatchable items reached the upstream: %+v", calls)
	}
}

// An item runs under the plans it started with: plans published while it runs (the submitter disabled, its fallback credential gone) do not reach it, so its failover still goes to the credential its route held.
func TestRun_KeepsThePlanItStartedWith(t *testing.T) {
	var swap func()
	up := newUpstream(t, func(n int) (int, string) {
		if n == 1 {
			swap()
			return http.StatusInternalServerError, ``
		}
		return http.StatusOK, `{"id":"x","usage":{"prompt_tokens":1,"completion_tokens":1}}`
	})

	var w plantest.World
	prov := w.Provider("vendor")
	h := w.Host("vendor", up.URL)
	m := w.Model(prov, "test-model")
	w.Bind(m, h, adapters.OpenAI)
	w.Credential(h, nil, plantest.System, "sk-first")
	fallback := w.Credential(h, nil, plantest.System, "sk-fallback")
	u := w.User()
	k := w.UserKey(u, "rk-secret")
	plans := w.Load(t)
	rn, _, _ := runnerOver(t, plans)

	swap = func() {
		gone := *fallback
		off := false
		gone.Spec.Enabled = &off
		plantest.Apply(t, plans, &gone)
		plantest.Apply(t, plans, &user.User{ID: u.ID, Username: u.Username, TokenVersion: u.TokenVersion, Disabled: true})
	}
	attr := Attribution{PrincipalKind: string(key.PrincipalUser), PrincipalID: u.ID, CredentialKind: inference.CredentialKey, CredentialID: k.Meta.ID}
	status, _, err := rn.Run(context.Background(), "item-1", k.Spec.KeyHash, TokenClaims{}, attr, adapters.OpenAI, []byte(itemBody))
	if err != nil || status != http.StatusOK {
		t.Fatalf("Run: status %d, err %v; want the fallback's 200 under the plan the item started with", status, err)
	}
	want := []sent{{"/v1/chat/completions", "Bearer sk-first", itemBody}, {"/v1/chat/completions", "Bearer sk-fallback", itemBody}}
	if d := cmp.Diff(want, up.sent()); d != "" {
		t.Fatalf("upstream calls (-want +got):\n%s", d)
	}
	if plans.Caller(t, k).Refused == nil {
		t.Fatal("the swapped plans did not refuse the disabled submitter; the test proves nothing")
	}
}

// An item in the family's shape, or in the member's own, runs on a route whose first hop binds a member of the family, failing over to a hop of the family's shape: each hop is sent the item in its own form, path and auth.
func TestRun_FamilyRoute(t *testing.T) {
	for _, inbound := range []adapters.Name{"shape-base", "shape-member"} {
		t.Run(string(inbound), func(t *testing.T) { runFamilyRoute(t, inbound) })
	}
}

func runFamilyRoute(t *testing.T, inbound adapters.Name) {
	member := newUpstream(t, answering(http.StatusInternalServerError, `{"error":{"message":"down"}}`))
	base := newUpstream(t, answering(http.StatusOK, `{"id":"x"}`))

	var w plantest.World
	m := w.Model(w.Provider("vendor"), "test-model")
	memberHost, baseHost := w.Host("member", member.URL), w.Host("base", base.URL)
	w.Bind(m, memberHost, "shape-member")
	w.Bind(m, baseHost, "shape-base")
	w.Credential(memberHost, nil, plantest.System, "sk-member")
	w.Credential(baseHost, nil, plantest.System, "sk-base")
	u := w.User()
	k := w.UserKey(u, "rk-secret")
	rn, _, _ := runnerOver(t, w.Load(t))
	rn.Specs = adapter.NewRegistry(
		(&adapter.Spec{Name: "shape-base", DefaultPath: "/base/generate", Auth: adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"}}).Build(),
		(&adapter.Spec{Name: "shape-member", Family: "shape-base", DefaultPath: "/member/generate", Auth: adapter.AuthStrategy{Header: "Authorization", Scheme: "Token"}, Envelope: &adapter.RequestEnvelope{
			DropMembers:    []string{"model"},
			DefaultMembers: []adapter.BodyMember{{Key: "version", Value: []byte(`"v1"`)}},
		}}).Build(),
	)

	attr := Attribution{PrincipalKind: string(key.PrincipalUser), PrincipalID: u.ID, CredentialKind: inference.CredentialKey, CredentialID: k.Meta.ID}
	if status, _, err := rn.Run(context.Background(), "item-1", k.Spec.KeyHash, TokenClaims{}, attr, inbound, []byte(itemBody)); err != nil || status != http.StatusOK {
		t.Fatalf("Run: status %d, err %v; want the base hop's 200", status, err)
	}
	if d := cmp.Diff([]sent{{"/member/generate", "Token sk-member", `{"version":"v1"}`}}, member.sent()); d != "" {
		t.Errorf("member hop (-want +got):\n%s", d)
	}
	if d := cmp.Diff([]sent{{"/base/generate", "Bearer sk-base", itemBody}}, base.sent()); d != "" {
		t.Errorf("base hop (-want +got):\n%s", d)
	}
}

// A Messages item runs on a cloud binding of the anthropic family as relay registers it: the model moves into the path and the version into the body.
func TestRun_CloudMessagesBinding(t *testing.T) {
	cloud := newUpstream(t, answering(http.StatusOK, `{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))

	var w plantest.World
	m := w.Model(w.Provider("vendor"), "test-model")
	h := w.Host("cloud", cloud.URL)
	h.Spec.Backend = map[string]string{"project": "p1", "location": "global"}
	b := w.Bind(m, h, adapters.VertexAnthropic)
	b.Spec.UpstreamName, b.Spec.Snapshots = "claude-test-1@20250101", []string{"test-model"}
	w.Credential(h, nil, plantest.System, "sk-cloud")
	u := w.User()
	k := w.UserKey(u, "rk-secret")
	rn, _, _ := runnerOver(t, w.Load(t))
	rn.Specs = adapter.NewRegistry(adapterspecs.Specs()...)

	attr := Attribution{PrincipalKind: string(key.PrincipalUser), PrincipalID: u.ID, CredentialKind: inference.CredentialKey, CredentialID: k.Meta.ID}
	item := `{"model":"test-model","max_tokens":16,"messages":[]}`
	if status, _, err := rn.Run(context.Background(), "item-1", k.Spec.KeyHash, TokenClaims{}, attr, adapters.Anthropic, []byte(item)); err != nil || status != http.StatusOK {
		t.Fatalf("Run: status %d, err %v; want 200", status, err)
	}
	want := sent{"/v1/projects/p1/locations/global/publishers/anthropic/models/claude-test-1@20250101:rawPredict", "Bearer sk-cloud", `{"max_tokens":16,"messages":[],"anthropic_version":"vertex-2023-10-16"}`}
	if d := cmp.Diff([]sent{want}, cloud.sent()); d != "" {
		t.Errorf("cloud hop (-want +got):\n%s", d)
	}
}

// A failover between hosts that name the model differently sends each its own name: the body is built for the first hop and reset for the next.
func TestRun_FailoverSendsEachHopItsUpstreamName(t *testing.T) {
	const wire = "global.vendor/test-model-v1:0"
	named := newUpstream(t, answering(http.StatusInternalServerError, `{"error":{"message":"down"}}`))
	direct := newUpstream(t, answering(http.StatusOK, `{"id":"x","usage":{"prompt_tokens":1,"completion_tokens":1}}`))

	var w plantest.World
	m := w.Model(w.Provider("vendor"), "test-model")
	cloud, vendor := w.Host("cloud", named.URL), w.Host("vendor", direct.URL)
	b := w.Bind(m, cloud, adapters.OpenAI)
	b.Spec.UpstreamName, b.Spec.Snapshots = wire, []string{"test-model"}
	w.Bind(m, vendor, adapters.OpenAI)
	w.Credential(cloud, nil, plantest.System, "sk-cloud")
	w.Credential(vendor, nil, plantest.System, "sk-vendor")
	u := w.User()
	k := w.UserKey(u, "rk-secret")
	rn, _, _ := runnerOver(t, w.Load(t))

	attr := Attribution{PrincipalKind: string(key.PrincipalUser), PrincipalID: u.ID, CredentialKind: inference.CredentialKey, CredentialID: k.Meta.ID}
	if status, _, err := rn.Run(context.Background(), "item-1", k.Spec.KeyHash, TokenClaims{}, attr, adapters.OpenAI, []byte(itemBody)); err != nil || status != http.StatusOK {
		t.Fatalf("Run: status %d, err %v; want the second hop's 200", status, err)
	}
	if d := cmp.Diff([]sent{{"/v1/chat/completions", "Bearer sk-cloud", `{"model":"` + wire + `"}`}}, named.sent()); d != "" {
		t.Errorf("named hop (-want +got):\n%s", d)
	}
	if d := cmp.Diff([]sent{{"/v1/chat/completions", "Bearer sk-vendor", itemBody}}, direct.sent()); d != "" {
		t.Errorf("direct hop (-want +got):\n%s", d)
	}
}
