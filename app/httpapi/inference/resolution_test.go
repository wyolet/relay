package inference

// resolution_test.go covers the two resolution decisions the data plane makes
// before a request reaches a runner: which policy the credential resolves to,
// and which (model, host) the request routes to.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/slug"
)

// twoHostDispatch builds a model served on two hosts — one whose key serves
// it and one whose upstream tier does not, so a routing answer names which
// host was picked — plus a third host that serves nothing at all.
func twoHostDispatch(t *testing.T) (*catalog.Catalog, *Principal) {
	t.Helper()
	provID, hostA, hostB, idleHost := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	modID, polID := meta.NewID(), meta.NewID()
	tierA, tierB := meta.NewID(), meta.NewID()
	hkA, hkB := meta.NewID(), meta.NewID()

	prov := &provider.Provider{Meta: meta.Metadata{ID: provID, Name: "acme", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	mkHost := func(id, name string) *host.Host {
		return &host.Host{
			Meta: meta.Metadata{ID: id, Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: host.Spec{BaseURL: "http://" + name + ".invalid"},
		}
	}
	mkTier := func(id, name, hostID string, grants ...string) *policy.Policy {
		return &policy.Policy{
			Meta: meta.Metadata{ID: id, Name: name, Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
			Spec: policy.Spec{Models: grants},
		}
	}
	mkKey := func(id, name, hostID, tier string) *hostkey.HostKey {
		return &hostkey.HostKey{
			Meta: meta.Metadata{ID: id, Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: hostkey.Spec{HostID: hostID, PolicyID: tier, Value: "sk", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
		}
	}
	m := &model.Model{
		Meta: meta.Metadata{ID: modID, Name: "test-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: slug.From("test-model")}}, Pointer: slug.From("test-model")},
	}
	mkBinding := func(name, hostID string) *binding.Binding {
		return &binding.Binding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: binding.Spec{ModelID: modID, HostID: hostID, Adapter: adapters.OpenAI},
		}
	}
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: polID, Name: "p", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{ModelIDs: []string{modID}, HostKeyIDs: []string{hkA, hkB}},
	}
	userID := meta.NewID()
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "rk", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalUser, ID: userID},
			PolicyID:  polID, KeyHash: sha("sk-models"),
		},
	}
	cat := catalog.New(
		provListD{prov},
		hostListD{mkHost(hostA, "host-a"), mkHost(hostB, "host-b"), mkHost(idleHost, "host-idle")},
		// host-b's tier grants a different model, so a request pinned there
		// resolves the binding and then finds no key it may spend.
		polListD{pol, mkTier(tierA, "tier-a", hostA), mkTier(tierB, "tier-b", hostB, "acme/something-else")},
		modListD{m},
		keyListD{mkKey(hkA, "key-a", hostA, tierA), mkKey(hkB, "key-b", hostB, tierB)},
		rlListD{}, rkListD{k}, rcListD{},
		bndListD{mkBinding("m-on-a", hostA), mkBinding("m-on-b", hostB)},
	)
	if err := cat.Reload(t.Context()); err != nil {
		t.Fatalf("catalog reload: %v", err)
	}
	return cat, &Principal{
		CredentialKind: CredentialKey, CredentialID: k.Meta.ID,
		KeyHash: k.Spec.KeyHash, Key: k, Policy: pol,
	}
}

// dispatchWith runs one request through Dispatch and returns the recorder.
func dispatchWith(t *testing.T, d Deps, pr *Principal, modelName string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	r = withNormalContext(r, pr)
	w := httptest.NewRecorder()
	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAI,
		Body:      []byte(`{"model":"` + modelName + `"}`),
		ModelName: modelName,
	})
	return w
}

// TestDispatch_UpstreamHostFold pins how the upstream-host header reaches
// routing: it is folded into the model ref as a host pin, and an "@host"
// already in the ref wins over it — the caller's explicit choice is not
// silently overridden by a header a proxy in front may have set.
func TestDispatch_UpstreamHostFold(t *testing.T) {
	cat, pr := twoHostDispatch(t)
	d := buildDeps(t, cat)

	for _, tc := range []struct {
		name      string
		modelName string
		header    string
		wantCode  string // "" means routing resolved and the stub upstream took over
	}{
		{name: "no header takes the binding whose key serves the model", modelName: "test-model"},
		{
			// host-b's binding exists, so the pin lands there; its tier is
			// what refuses, which proves the header reached routing.
			name:      "the header pins a host whose tier denies the model",
			modelName: "test-model", header: "host-b",
			wantCode: "no_keys",
		},
		{
			name:      "the header pins a host with no binding for the model",
			modelName: "test-model", header: "host-idle",
			wantCode: "model_not_found",
		},
		{
			name:      "an explicit pin in the ref wins over the header",
			modelName: "test-model@host-a", header: "host-b",
		},
		{
			name:      "the explicit pin still decides when it is the narrower one",
			modelName: "test-model@host-b", header: "host-a",
			wantCode: "no_keys",
		},
		{
			name:      "an explicit pin to a host with no binding loses the model",
			modelName: "test-model@host-idle", header: "host-a",
			wantCode: "model_not_found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{}
			if tc.header != "" {
				headers[httpheader.HeaderUpstreamHost] = tc.header
			}
			w := dispatchWith(t, d, pr, tc.modelName, headers)
			if tc.wantCode == "" {
				// Routing succeeded; the stub upstream is what fails next, and
				// it never answers a routing code.
				switch got := parseDispatchErr(t, w.Body.Bytes()).Error.Code; got {
				case "model_not_found", "no_keys", "no_host_binding", "model_not_allowed":
					t.Fatalf("routing rejected the request: %q (%s)", got, w.Body)
				}
				return
			}
			if got := parseDispatchErr(t, w.Body.Bytes()).Error.Code; got != tc.wantCode {
				t.Fatalf("error code = %q, want %q (status %d, body %s)", got, tc.wantCode, w.Code, w.Body)
			}
		})
	}
}

// A header naming a host that does not exist at all is treated the same as
// one naming an unrelated host: the pinned ref resolves to nothing rather
// than silently falling back to an unpinned lookup.
func TestDispatch_UpstreamHostHeaderNamingNoHost(t *testing.T) {
	cat, pr := twoHostDispatch(t)
	d := buildDeps(t, cat)
	w := dispatchWith(t, d, pr, "test-model", map[string]string{httpheader.HeaderUpstreamHost: "not-a-host"})
	if got := parseDispatchErr(t, w.Body.Bytes()).Error.Code; got != "model_not_found" {
		t.Fatalf("error code = %q, want model_not_found (body %s)", got, w.Body)
	}
}

// TestPolicyResolution_TiesAndDisabled covers the corners of the binding walk
// the ordered table in TestPolicyResolution does not. That table already pins
// the order itself — a key's own policy first, then the service account's
// override, then the bindings — so what is left is how the binding list is
// ordered within that last step, and what a binding pointing at a policy that
// is switched off or gone does to the walk (D77).
func TestPolicyResolution_TiesAndDisabled(t *testing.T) {
	const subject = "group:system:authenticated"

	t.Run("a priority tie is broken by binding name", func(t *testing.T) {
		f := newPrincipalFixture()
		f.sa.Spec.PolicyID = ""
		f.bindings = []*policybinding.PolicyBinding{
			boundTo(f, "zulu", 10, f.keyPol.Meta.ID, subject),
			boundTo(f, "alpha", 10, f.boundPol.Meta.ID, subject),
		}
		st := f.stack(t, saKey(f, "sk-tie"))
		if rec := st.do("sk-tie"); rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		if st.seen.Policy == nil || st.seen.Policy.Meta.ID != f.boundPol.Meta.ID {
			t.Fatalf("policy = %+v, want the alphabetically first binding of the tied pair", st.seen.Policy)
		}
	})

	t.Run("an explicit priority zero orders ahead of the default", func(t *testing.T) {
		f := newPrincipalFixture()
		f.sa.Spec.PolicyID = ""
		f.bindings = []*policybinding.PolicyBinding{
			boundTo(f, "aaa-default", policybinding.DefaultPriority, f.keyPol.Meta.ID, subject),
			boundTo(f, "zzz-explicit-zero", 0, f.boundPol.Meta.ID, subject),
		}
		st := f.stack(t, saKey(f, "sk-zero"))
		if rec := st.do("sk-zero"); rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		if st.seen.Policy == nil || st.seen.Policy.Meta.ID != f.boundPol.Meta.ID {
			t.Fatalf("policy = %+v, want the priority-zero binding", st.seen.Policy)
		}
	})

	t.Run("a binding naming a policy that is gone is skipped", func(t *testing.T) {
		f := newPrincipalFixture()
		f.sa.Spec.PolicyID = ""
		f.bindings = []*policybinding.PolicyBinding{
			boundTo(f, "aaa-dangling", 1, meta.NewID(), subject),
			boundTo(f, "bbb-live", 2, f.boundPol.Meta.ID, subject),
		}
		st := f.stack(t, saKey(f, "sk-dangling"))
		if rec := st.do("sk-dangling"); rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		if st.seen.Policy == nil || st.seen.Policy.Meta.ID != f.boundPol.Meta.ID {
			t.Fatalf("policy = %+v, want the binding whose policy still exists", st.seen.Policy)
		}
	})

	// A disabled binding that orders FIRST stops the walk with 403
	// policy_disabled; that half is pinned by
	// TestPrincipal_DisabledBindingDoesNotFallThrough. What is left is the
	// other side: ordering behind a live binding, it never decides at all.
	t.Run("a disabled binding later in the order never decides", func(t *testing.T) {
		f := newPrincipalFixture()
		f.sa.Spec.PolicyID = ""
		off := false
		f.keyPol.Spec.Enabled = &off
		f.bindings = []*policybinding.PolicyBinding{
			boundTo(f, "aaa-live", 1, f.boundPol.Meta.ID, subject),
			boundTo(f, "bbb-disabled", 2, f.keyPol.Meta.ID, subject),
		}
		st := f.stack(t, saKey(f, "sk-later"))
		if rec := st.do("sk-later"); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
		}
		if st.seen.Policy == nil || st.seen.Policy.Meta.ID != f.boundPol.Meta.ID {
			t.Fatalf("policy = %+v, want the live binding that ordered first", st.seen.Policy)
		}
	})
}

// The listing serves exactly what the caller's policy grants: a model the
// policy does not name is not advertised, and neither is one whose bindings
// declare no matching wire shape.
func TestListModels_PolicyBoundMatchesTheGrant(t *testing.T) {
	cat, pr := twoHostDispatch(t)
	d := buildDeps(t, cat)
	ctx := context.WithValue(context.Background(), ctxPrincipalT{}, pr)

	out, err := listModels(ctx, d, "")
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	if len(out.Body.Data) != 1 || out.Body.Data[0].ID != "test-model" {
		t.Fatalf("models = %+v, want the granted model once", out.Body.Data)
	}
	if out.Body.Object != "list" || out.Body.Data[0].Object != "model" || out.Body.Data[0].OwnedBy != "acme" {
		t.Errorf("row shape = %+v", out.Body.Data[0])
	}

	// A wire shape no binding declares hides the model entirely.
	narrowed, err := listModels(ctx, d, adapters.Anthropic)
	if err != nil {
		t.Fatalf("listModels with an adapter filter: %v", err)
	}
	if len(narrowed.Body.Data) != 0 {
		t.Fatalf("models = %+v, want none reachable over that wire shape", narrowed.Body.Data)
	}

	// A policy granting nothing lists nothing.
	empty := *pr
	empty.Policy = &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "empty", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{Models: []string{"other/model"}},
	}
	out, err = listModels(context.WithValue(context.Background(), ctxPrincipalT{}, &empty), d, "")
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	if len(out.Body.Data) != 0 {
		t.Fatalf("models = %+v, want none", out.Body.Data)
	}
}

// The listing is reachable over HTTP at both mounted paths, and the OpenAI
// namespace filters to models a binding serves over that wire shape.
func TestListModels_OverTheMountedRoutes(t *testing.T) {
	cat, _ := twoHostDispatch(t)
	d := buildDeps(t, cat)
	d.Tokens = &TokenVerifier{}

	r := chi.NewRouter()
	Mount(r, d)

	for _, path := range []string{"/v1/models", "/openai/v1/models"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req = req.WithContext(WithClassification(req.Context(), Classification{Mode: ModeNormal}))
			req.Header.Set("Authorization", "Bearer sk-models")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
			}
			var out struct {
				Object string `json:"object"`
				Data   []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode %s: %v", w.Body, err)
			}
			if out.Object != "list" {
				t.Errorf("object = %q, want list", out.Object)
			}
			if len(out.Data) == 0 {
				t.Fatal("the listing is empty; both routes serve the granted model")
			}
		})
	}
}

// Without a credential there is nothing to list; with a policy-less one the
// listing is refused unless the operator opened policy-less traffic, so it
// can never advertise more than the request path would serve.
func TestListModels_RejectionsMatchTheRequestPath(t *testing.T) {
	cat, pr := twoHostDispatch(t)
	d := buildDeps(t, cat)

	if _, err := listModels(context.Background(), d, ""); err == nil {
		t.Fatal("listModels with no principal returned a listing")
	}
	policyless := *pr
	policyless.Policy = nil
	if _, err := listModels(context.WithValue(context.Background(), ctxPrincipalT{}, &policyless), d, ""); err == nil {
		t.Fatal("listModels served a policy-less caller while policy-less traffic is off")
	}
}
