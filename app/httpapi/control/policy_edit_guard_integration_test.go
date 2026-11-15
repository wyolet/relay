//go:build integration

package control

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// tierWorld adds to the policy world a tier preset of the share host linking one of the host's rate limits, bound to team one's account by a team-one binding. Dave edits tiers everywhere (a custom role at the system scope), administers team one and only views team two, whose account is "Research".
type tierWorld struct {
	*policyWorld
	tier             *policy.Policy
	tierRPM          *ratelimit.RateLimit
	oneAcct, twoAcct string
	bindOne          *policybinding.PolicyBinding
}

func newTierWorld(t *testing.T, opts ...func(*Deps)) *tierWorld {
	t.Helper()
	ctx := t.Context()
	w := &tierWorld{policyWorld: newPolicyWorld(t, opts...)}
	hostOwner := meta.Owner{Kind: meta.OwnerHost, ID: w.host.Meta.ID}
	w.tierRPM = &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: ids.New(), Name: "tier-rpm", Owner: hostOwner},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{Meter: ratelimit.MeterRequests, Amount: 10, Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategyFixedWindow}}},
	}
	must(t, "rate limit", w.stores.RateLimit.Upsert(ctx, w.tierRPM))
	w.tier = &policy.Policy{
		Meta: meta.Metadata{ID: ids.New(), Name: "tier", DisplayName: "Tier", Owner: hostOwner},
		Spec: policy.Spec{Models: []string{}, RateLimits: []policy.RateLimitLink{{RateLimitID: w.tierRPM.Meta.ID}}},
	}
	must(t, "tier policy", w.stores.Policy.Upsert(ctx, w.tier))

	var err error
	w.oneAcct, err = w.stores.Account.EnsureManaged(ctx, account.ForTeam(w.teamOne.Meta.ID, "one", "Platform"))
	must(t, "team one's account", err)
	w.twoAcct, err = w.stores.Account.EnsureManaged(ctx, account.ForTeam(w.teamTwo.Meta.ID, "two", "Research"))
	must(t, "team two's account", err)
	w.bindOne = w.bindTier(t, "tier-one", meta.Owner{Kind: meta.OwnerTeam, ID: w.teamOne.Meta.ID}, w.oneAcct)

	editor := &role.Role{
		Meta: meta.Metadata{ID: ids.New(), Name: "tier-editor", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: role.Spec{Rules: []role.Rule{
			{Kinds: []string{"policies", "rate-limits", "budgets"}, Verbs: []string{"get", "list", "create", "update"}},
			{Kinds: []string{"system"}, Verbs: []string{"apply"}},
		}},
	}
	must(t, "tier editor role", w.stores.Role.Upsert(ctx, editor))
	yes := true
	system := meta.Owner{Kind: meta.OwnerSystem}
	rb := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: ids.New(), Name: "dave-tier-editor", Owner: system}}
	rb.Spec.RoleID, rb.Spec.Scope, rb.Spec.Enabled = editor.Meta.ID, system, &yes
	rb.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: w.users["dave"]}}
	must(t, "role binding", w.stores.RoleBinding.Upsert(ctx, rb))
	must(t, "reload", w.cat.Reload(ctx))
	return w
}

func (w *tierWorld) bindTier(t *testing.T, name string, owner meta.Owner, accountID string) *policybinding.PolicyBinding {
	t.Helper()
	b := &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: owner},
		Spec: policybinding.Spec{PolicyID: w.tier.Meta.ID, AccountIDs: []string{accountID}},
	}
	must(t, "binding "+name, w.stores.PolicyBinding.Upsert(t.Context(), b))
	must(t, "reload", w.cat.Reload(t.Context()))
	return b
}

// bindTwo binds the tier to team two's account too.
func (w *tierWorld) bindTwo(t *testing.T) {
	t.Helper()
	w.bindTier(t, "tier-two", meta.Owner{Kind: meta.OwnerTeam, ID: w.teamTwo.Meta.ID}, w.twoAcct)
}

func (w *tierWorld) editTier(who string) *httptest.ResponseRecorder {
	return w.send(who, http.MethodPut, "/policies/by-id/"+w.tier.Meta.ID,
		`{"metadata":{"name":"tier","displayName":"Tier"},"spec":{"enabled":false,"models":[],"rateLimits":[{"rateLimitId":"`+w.tierRPM.Meta.ID+`"}]}}`)
}

func (w *tierWorld) editRPM(who string) *httptest.ResponseRecorder {
	return w.send(who, http.MethodPut, "/rate-limits/by-id/"+w.tierRPM.Meta.ID,
		`{"metadata":{"name":"tier-rpm"},"spec":{"rules":[{"meter":"requests","amount":20,"window":60,"strategy":"fixed-window"}]}}`)
}

type policyInUseAnswer struct {
	Error struct {
		Code, Message string
	} `json:"error"`
	BlockedBy policy.EditBlockers `json:"blockedBy"`
}

func wantPolicyInUse(t *testing.T, rec *httptest.ResponseRecorder, message string, accounts []string) {
	t.Helper()
	wantStatus(t, rec, http.StatusForbidden)
	var got policyInUseAnswer
	must(t, "decode", json.Unmarshal(rec.Body.Bytes(), &got))
	if got.Error.Code != codePolicyInUse || got.Error.Message != message {
		t.Fatalf("refusal = %s %q, want %s %q", got.Error.Code, got.Error.Message, codePolicyInUse, message)
	}
	var ids []string
	for _, a := range got.BlockedBy.Accounts {
		ids = append(ids, a.ID)
	}
	if !slices.Equal(ids, accounts) || got.BlockedBy.Hidden != 0 {
		t.Fatalf("blockedBy = %+v, want accounts %v", got.BlockedBy, accounts)
	}
}

func (w *tierWorld) storedTier(t *testing.T) *policy.Policy {
	t.Helper()
	p, err := w.stores.Policy.Get(t.Context(), w.tier.Meta.ID)
	must(t, "get tier", err)
	return p
}

func (w *tierWorld) storedAmount(t *testing.T) int64 {
	t.Helper()
	rl, err := w.stores.RateLimit.Get(t.Context(), w.tierRPM.Meta.ID)
	must(t, "get rate limit", err)
	return rl.Spec.Rules[0].Amount
}

// An edit of a policy, or of a rate limit it links, goes through while every account it reaches is one the editor may set the policy of. Bound also to an account of a team they only view, it answers 403 policy_in_use naming that account and writes nothing; an admin's edit still lands.
func TestPolicyEdit_RefusedWhileAppliedToAccountsTheEditorMayNotSet(t *testing.T) {
	t.Run("bound only where the editor may set the policy", func(t *testing.T) {
		w := newTierWorld(t)
		wantStatus(t, w.editRPM("dave"), http.StatusOK)
		if w.storedAmount(t) != 20 {
			t.Fatal("rate limit edit did not land")
		}
		wantStatus(t, w.editTier("dave"), http.StatusOK)
		if w.storedTier(t).IsEnabled() {
			t.Fatal("policy edit did not land")
		}
	})

	t.Run("bound also to another team's account", func(t *testing.T) {
		w := newTierWorld(t)
		w.bindTwo(t)
		wantPolicyInUse(t, w.editTier("dave"),
			`you can't change this policy: it is applied to team "Research", whose policy you may not set; duplicate it and edit the copy`,
			[]string{w.twoAcct})
		if !w.storedTier(t).IsEnabled() {
			t.Fatal("refused policy edit was written")
		}
		wantPolicyInUse(t, w.editRPM("dave"),
			`you can't change this rate limit: it is linked from policy "Tier", applied to team "Research", whose policy you may not set; duplicate the policy and link a new rate limit to the copy`,
			[]string{w.twoAcct})
		if w.storedAmount(t) != 10 {
			t.Fatal("refused rate limit edit was written")
		}

		wantStatus(t, w.editRPM("root"), http.StatusOK)
		wantStatus(t, w.editTier("root"), http.StatusOK)
		if w.storedTier(t).IsEnabled() || w.storedAmount(t) != 20 {
			t.Fatal("admin edits did not land")
		}
	})

	// The checks on the bindings are probes: the request's audit row is the policy's update.
	t.Run("the audit row names the policy", func(t *testing.T) {
		sink := &auditSink{}
		em := audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
		w := newTierWorld(t, func(d *Deps) { d.Audit = em })
		wantStatus(t, w.editTier("dave"), http.StatusOK)
		em.Close()
		evs := sink.all()
		if len(evs) != 1 || evs[0].Action != "policies.update" || evs[0].Resource.ID != w.tier.Meta.ID {
			t.Fatalf("audit events = %+v, want one policies.update of the tier", evs)
		}
	})

	// A disabled binding applies the policy to nobody.
	t.Run("a disabled binding does not block", func(t *testing.T) {
		w := newTierWorld(t)
		w.bindTwo(t)
		no := false
		all, err := w.stores.PolicyBinding.List(t.Context())
		must(t, "list", err)
		for _, b := range all {
			if b.Spec.PolicyID == w.tier.Meta.ID && b.Meta.Owner.ID == w.teamTwo.Meta.ID {
				b.Spec.Enabled = &no
				must(t, "disable", w.stores.PolicyBinding.Upsert(t.Context(), b))
			}
		}
		must(t, "reload", w.cat.Reload(t.Context()))
		wantStatus(t, w.editTier("dave"), http.StatusOK)
	})
}

// POST /apply refuses the same edits, naming the document; a dry run too.
func TestPolicyEdit_ApplyRefusedPerDocument(t *testing.T) {
	w := newTierWorld(t)
	w.bindTwo(t)
	bundle := "apiVersion: relay.wyolet.dev/v1alpha3\nkind: Policy\nmetadata: {name: tier, displayName: Tier, owner: {kind: host, name: share-host}}\nspec: {enabled: false, rateLimits: [{ref: tier-rpm}]}\n"
	for _, path := range []string{"/apply?dryRun=true", "/apply"} {
		rec := w.sendYAML("dave", path, bundle)
		wantPolicyInUse(t, rec,
			`apply: Policy "tier": you can't change this policy: it is applied to team "Research", whose policy you may not set; duplicate it and edit the copy`,
			[]string{w.twoAcct})
	}
	if !w.storedTier(t).IsEnabled() {
		t.Fatal("refused apply was written")
	}
	rlBundle := "apiVersion: relay.wyolet.dev/v1alpha3\nkind: RateLimit\nmetadata: {name: tier-rpm, owner: {kind: host, name: share-host}}\nspec: {rules: [{meter: requests, amount: 30, window: 60, strategy: fixed-window}]}\n"
	wantPolicyInUse(t, w.sendYAML("dave", "/apply", rlBundle),
		`apply: RateLimit "tier-rpm": you can't change this rate limit: it is linked from policy "Tier", applied to team "Research", whose policy you may not set; duplicate the policy and link a new rate limit to the copy`,
		[]string{w.twoAcct})
	if w.storedAmount(t) != 10 {
		t.Fatal("refused rate limit apply was written")
	}
	wantStatus(t, w.sendYAML("root", "/apply", bundle), http.StatusOK)
	if w.storedTier(t).IsEnabled() {
		t.Fatal("admin apply did not land")
	}
}

func (w *tierWorld) sendYAML(who, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/yaml")
	req.Header.Set("X-Test-User", who)
	rec := httptest.NewRecorder()
	w.r.ServeHTTP(rec, req)
	return rec
}

func (w *tierWorld) readTier(t *testing.T, who string) policy.Policy {
	t.Helper()
	rec := w.send(who, http.MethodGet, "/policies/"+w.tier.Meta.ID, "")
	wantStatus(t, rec, http.StatusOK)
	var p policy.Policy
	must(t, "decode policy", json.Unmarshal(rec.Body.Bytes(), &p))
	return p
}

// A read of one policy says whether the caller may update it and which accounts stop them, as the update would decide.
func TestPolicyEdit_ReadShowsEditable(t *testing.T) {
	w := newTierWorld(t)
	editable := func(p policy.Policy) bool { return p.Editable != nil && *p.Editable }

	if p := w.readTier(t, "dave"); !editable(p) || p.EditBlockedBy != nil {
		t.Fatalf("dave, bound to team one only: editable %v, blocked by %+v", p.Editable, p.EditBlockedBy)
	}
	w.bindTwo(t)
	p := w.readTier(t, "dave")
	if editable(p) || p.EditBlockedBy == nil {
		t.Fatalf("dave, bound to team two too: editable %v, blocked by %+v", p.Editable, p.EditBlockedBy)
	}
	want := []policy.AccountRef{{ID: w.twoAcct, Name: "team-two", DisplayName: "Research", Kind: "team"}}
	if !slices.Equal(p.EditBlockedBy.Accounts, want) || p.EditBlockedBy.Hidden != 0 {
		t.Fatalf("blocked by %+v, want %+v", p.EditBlockedBy, want)
	}
	if p := w.readTier(t, "root"); !editable(p) || p.EditBlockedBy != nil {
		t.Fatalf("admin: editable %v, blocked by %+v", p.Editable, p.EditBlockedBy)
	}

	// Lists leave it out.
	rec := w.send("dave", http.MethodGet, "/policies", "")
	wantStatus(t, rec, http.StatusOK)
	var list struct{ Items []policy.Policy }
	must(t, "decode list", json.Unmarshal(rec.Body.Bytes(), &list))
	for _, it := range list.Items {
		if it.Editable != nil || it.EditBlockedBy != nil {
			t.Fatalf("list item %s carries editable %v, blocked by %+v", it.Meta.Name, it.Editable, it.EditBlockedBy)
		}
	}
}

// A copy has a new id, the source's spec and links, the owner the create rules allow, and no binding; the caller then edits it freely. It is audited as a create.
func TestPolicyDuplicate(t *testing.T) {
	sink := &auditSink{}
	em := audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
	w := newTierWorld(t, func(d *Deps) { d.Audit = em })
	w.bindTwo(t)
	teamOne := meta.Owner{Kind: meta.OwnerTeam, ID: w.teamOne.Meta.ID}

	rec := w.send("dave", http.MethodPost, "/policies/tier/duplicate", `{"displayName":"Platform tier","owner":{"kind":"team","id":"`+teamOne.ID+`"}}`)
	wantStatus(t, rec, http.StatusCreated)
	var cp policy.Policy
	must(t, "decode copy", json.Unmarshal(rec.Body.Bytes(), &cp))
	if cp.Meta.ID == w.tier.Meta.ID || cp.Meta.Name != "platform-tier" || cp.Meta.DisplayName != "Platform tier" || cp.Meta.Owner != teamOne {
		t.Fatalf("copy metadata = %+v", cp.Meta)
	}
	if len(cp.Spec.RateLimits) != 1 || cp.Spec.RateLimits[0].RateLimitID != w.tierRPM.Meta.ID || !cp.IsEnabled() {
		t.Fatalf("copy spec = %+v, want the tier's links", cp.Spec)
	}
	if cp.Editable == nil || !*cp.Editable {
		t.Fatalf("copy editable = %v, want true", cp.Editable)
	}
	all, err := w.stores.PolicyBinding.List(t.Context())
	must(t, "list bindings", err)
	for _, b := range all {
		if b.Spec.PolicyID == cp.Meta.ID {
			t.Fatalf("copy is bound by %s", b.Meta.Name)
		}
	}

	// Omitted, the name follows the source's and the owner is the caller.
	rec = w.send("dave", http.MethodPost, "/policies/"+w.tier.Meta.ID+"/duplicate", "")
	wantStatus(t, rec, http.StatusCreated)
	var mine policy.Policy
	must(t, "decode default copy", json.Unmarshal(rec.Body.Bytes(), &mine))
	if mine.Meta.DisplayName != "Tier copy" || mine.Meta.Name != "tier-copy" || mine.Meta.Owner != (meta.Owner{Kind: meta.OwnerUser, ID: w.users["dave"]}) {
		t.Fatalf("default copy metadata = %+v", mine.Meta)
	}

	// The owner rules of a create: erin views team one, so she reads its policy but may not create one there; nobody names the system or another user; a source the caller may not read is not found.
	rec = w.send("erin", http.MethodPost, "/policies/"+w.polA.Meta.ID+"/duplicate", `{"owner":{"kind":"team","id":"`+teamOne.ID+`"}}`)
	wantStatus(t, rec, http.StatusForbidden)
	rec = w.send("dave", http.MethodPost, "/policies/tier/duplicate", `{"owner":{"kind":"system"}}`)
	wantStatus(t, rec, http.StatusBadRequest)
	rec = w.send("dave", http.MethodPost, "/policies/tier/duplicate", `{"owner":{"kind":"user","id":"`+w.users["alice"]+`"}}`)
	wantStatus(t, rec, http.StatusBadRequest)
	rec = w.send("carol", http.MethodPost, "/policies/"+w.polA.Meta.ID+"/duplicate", "")
	wantStatus(t, rec, http.StatusNotFound)

	rec = w.send("dave", http.MethodPut, "/policies/by-id/"+cp.Meta.ID, `{"metadata":{"name":"platform-tier","displayName":"Platform tier"},"spec":{"enabled":false,"models":[],"rateLimits":[{"rateLimitId":"`+w.tierRPM.Meta.ID+`"}]}}`)
	wantStatus(t, rec, http.StatusOK)

	em.Close()
	var creates int
	for _, ev := range sink.all() {
		if ev.Action == "policies.create" && ev.Outcome.Status == audit.StatusAllowed && ev.Outcome.Code == http.StatusCreated {
			if ev.Change == nil || !slices.Equal(ev.Change.Fields, []string{audit.AnyField}) {
				t.Errorf("create event changed %+v, want every field", ev.Change)
			}
			creates++
		}
	}
	if creates != 2 {
		t.Fatalf("%d allowed policies.create events, want 2 (one per copy)", creates)
	}
}

// A copy into the organization is an admin's to make: dave's role at the system scope does not reach it.
func TestPolicyDuplicateIntoOrganization(t *testing.T) {
	w := newTierWorld(t)
	org := `{"displayName":"Org tier","owner":{"kind":"organization"}}`

	wantStatus(t, w.send("dave", http.MethodPost, "/policies/tier/duplicate", org), http.StatusForbidden)
	rec := w.send("root", http.MethodPost, "/policies/tier/duplicate", org)
	wantStatus(t, rec, http.StatusCreated)
	var cp policy.Policy
	must(t, "decode copy", json.Unmarshal(rec.Body.Bytes(), &cp))
	if cp.Meta.Owner != (meta.Owner{Kind: meta.OwnerOrganization}) || cp.Meta.Name != "org-tier" {
		t.Fatalf("copy metadata = %+v", cp.Meta)
	}
	wantStatus(t, w.send("dave", http.MethodGet, "/policies/"+cp.Meta.ID, ""), http.StatusNotFound)
}
