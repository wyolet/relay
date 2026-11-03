//go:build integration

package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/ids"
)

// sharesWorld is the control API in roles mode over Postgres: alice owns a personal provider account with a key; team one owns one, administered by dave, with erin as a viewer; bob administers team two, which holds project two and its service account; carol holds no role.
type sharesWorld struct {
	r                    http.Handler
	stores               *appcatalog.Stores
	cat                  *appcatalog.Catalog
	usage                *fakeUsageReader
	users                map[string]string
	host                 *host.Host
	alicesAcct, teamAcct *account.Account
	aliceKey             *hostkey.HostKey
	teamOne, teamTwo     *team.Team
	projTwo              *project.Project
	sa                   *serviceaccount.ServiceAccount
}

func newSharesWorld(t *testing.T, opts ...func(*Deps)) *sharesWorld {
	t.Helper()
	ctx := t.Context()
	pgPool := storagetest.Pool(t)
	cat, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pgPool})
	must(t, "bootstrap", err)
	must(t, "seed roles", role.SeedBuiltins(ctx, stores.Role, nil, nil))
	roles, err := stores.Role.List(ctx)
	must(t, "list roles", err)
	roleID := map[string]string{}
	for _, r := range roles {
		roleID[r.Meta.Name] = r.Meta.ID
	}

	w := &sharesWorld{stores: stores, cat: cat, usage: &fakeUsageReader{}, users: map[string]string{}}
	users := user.NewStore(pgPool)
	for _, name := range []string{"alice", "bob", "carol", "dave", "erin"} {
		id := ids.New()
		w.users[name] = id
		must(t, "upsert user", users.Upsert(ctx, &user.User{ID: id, Username: name}))
	}
	system := meta.Owner{Kind: meta.OwnerSystem}
	w.host = &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "share-host", Owner: system}, Spec: host.Spec{BaseURL: "https://share.example.com"}}
	must(t, "host", stores.Host.Upsert(ctx, w.host))
	w.teamOne = &team.Team{Meta: meta.Metadata{ID: ids.New(), Name: "one", Owner: system}}
	w.teamTwo = &team.Team{Meta: meta.Metadata{ID: ids.New(), Name: "two", Owner: system}}
	for _, tm := range []*team.Team{w.teamOne, w.teamTwo} {
		must(t, "team", stores.Team.Upsert(ctx, tm))
	}
	w.projTwo = &project.Project{Meta: meta.Metadata{ID: ids.New(), Name: "two-app"}, Spec: project.Spec{TeamID: w.teamTwo.Meta.ID}}
	w.projTwo.StampOwner()
	must(t, "project", stores.Project.Upsert(ctx, w.projTwo))
	w.sa = &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: ids.New(), Name: "two-bot"}, Spec: serviceaccount.Spec{ProjectID: w.projTwo.Meta.ID}}
	w.sa.StampOwner()
	must(t, "service account", stores.ServiceAccount.Upsert(ctx, w.sa))

	alice := meta.Owner{Kind: meta.OwnerUser, ID: w.users["alice"]}
	w.alicesAcct = &account.Account{Meta: meta.Metadata{ID: ids.New(), Name: "alice-anthropic", Owner: alice}, Spec: account.Spec{HostID: w.host.Meta.ID}}
	w.teamAcct = &account.Account{Meta: meta.Metadata{ID: ids.New(), Name: "one-anthropic", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: w.teamOne.Meta.ID}}, Spec: account.Spec{HostID: w.host.Meta.ID}}
	for _, a := range []*account.Account{w.alicesAcct, w.teamAcct} {
		must(t, "account", stores.Account.Upsert(ctx, a))
	}
	w.aliceKey = &hostkey.HostKey{Meta: meta.Metadata{ID: ids.New(), Name: "alice-key", Owner: alice}, Spec: hostkey.Spec{HostID: w.host.Meta.ID, AccountID: w.alicesAcct.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "SHARE_TEST_KEY"}}}
	must(t, "host key", stores.HostKey.Upsert(ctx, w.aliceKey))

	yes := true
	for _, g := range []struct {
		who, role string
		scope     meta.Owner
	}{
		{"dave", "team-admin", meta.Owner{Kind: meta.OwnerTeam, ID: w.teamOne.Meta.ID}},
		{"erin", "viewer", meta.Owner{Kind: meta.OwnerTeam, ID: w.teamOne.Meta.ID}},
		{"bob", "team-admin", meta.Owner{Kind: meta.OwnerTeam, ID: w.teamTwo.Meta.ID}},
		// Sharers see who they share with: alice and dave view team two, where bob is.
		{"alice", "viewer", meta.Owner{Kind: meta.OwnerTeam, ID: w.teamTwo.Meta.ID}},
		{"dave", "viewer", meta.Owner{Kind: meta.OwnerTeam, ID: w.teamTwo.Meta.ID}},
	} {
		rb := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: ids.New(), Name: g.who + "-" + g.role + "-" + g.scope.ID, Owner: g.scope}}
		rb.Spec.RoleID, rb.Spec.Scope, rb.Spec.Enabled = roleID[g.role], g.scope, &yes
		rb.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: w.users[g.who]}}
		must(t, "role binding", stores.RoleBinding.Upsert(ctx, rb))
		// The account writer runs with the plans; here the member accounts are made directly.
		_, err := stores.Account.EnsureManaged(ctx, account.Member(w.users[g.who], g.who, g.scope, "scope", ""))
		must(t, "member account", err)
	}
	must(t, "reload", cat.Reload(ctx))

	deps := mountDeps(t)
	deps.Stores, deps.Catalog, deps.Users, deps.UsageReader = stores, cat, users, w.usage
	deps.Authz = audit.Authorizer{Inner: authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}, Snap: cat.Current}
	actors := map[string]*actor.Actor{"root": {AdminToken: true, Username: "admin-token"}}
	for name, id := range w.users {
		actors[name] = &actor.Actor{UserID: id, Username: name, SessionID: "s-" + name, Subjects: appcatalog.UserSubjects(id, nil, nil)}
	}
	for _, opt := range opts {
		opt(&deps)
	}
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-User", actors))
	Mount(r, deps)
	w.r = r
	return w
}

func (w *sharesWorld) send(who, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", who)
	rec := httptest.NewRecorder()
	w.r.ServeHTTP(rec, req)
	return rec
}

func (w *sharesWorld) share(who string, src *account.Account, kind account.ReceiverKind, id string) *httptest.ResponseRecorder {
	return w.send(who, http.MethodPost, "/accounts/"+src.Meta.ID+"/shares", `{"receiver":{"kind":"`+string(kind)+`","id":"`+id+`"}}`)
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error.Code
}

func decodeAccount(t *testing.T, rec *httptest.ResponseRecorder) account.Account {
	t.Helper()
	var a account.Account
	must(t, "decode account", json.Unmarshal(rec.Body.Bytes(), &a))
	return a
}

// Only the real account's owner shares it: its user, or an admin of its team. Viewers and strangers are refused, a second share with one receiver and a share of a share answer 409, and the share is owned by its receiver.
func TestShares_CreateAndRules(t *testing.T) {
	w := newSharesWorld(t)
	bob := w.users["bob"]

	rec := w.share("alice", w.alicesAcct, account.ReceiverUser, bob)
	wantStatus(t, rec, http.StatusCreated)
	toBob := decodeAccount(t, rec)
	if toBob.Spec.SourceAccountID != w.alicesAcct.Meta.ID || toBob.Meta.Owner != (meta.Owner{Kind: meta.OwnerUser, ID: bob}) || toBob.Spec.Kind != account.KindProvider || toBob.Meta.Name != "alice-anthropic-for-bob" {
		t.Fatalf("share = %+v %+v", toBob.Meta, toBob.Spec)
	}
	if sb := toBob.Spec.SharedBy; sb == nil || sb.Kind != "user" || sb.ID != w.users["alice"] || sb.Name != "alice" {
		t.Fatalf("sharedBy = %+v, want alice", sb)
	}
	if rec := w.share("alice", w.alicesAcct, account.ReceiverUser, bob); rec.Code != http.StatusConflict || errorCode(t, rec) != "share_exists" {
		t.Fatalf("second share: %d %s, want 409 share_exists", rec.Code, rec.Body)
	}
	if rec := w.share("carol", w.alicesAcct, account.ReceiverUser, w.users["carol"]); rec.Code != http.StatusNotFound {
		t.Fatalf("a stranger's share: %d %s, want 404", rec.Code, rec.Body)
	}
	// A receiver the sharer cannot see answers as an unknown one, so ids cannot be probed.
	if rec := w.share("alice", w.alicesAcct, account.ReceiverUser, w.users["carol"]); rec.Code != http.StatusNotFound {
		t.Fatalf("a share with a user the sharer cannot see: %d %s, want 404", rec.Code, rec.Body)
	}
	if rec := w.share("alice", w.alicesAcct, account.ReceiverTeam, w.teamOne.Meta.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("a share with a team the sharer cannot see: %d %s, want 404", rec.Code, rec.Body)
	}
	// Only its user shares a personal account; the admin token does not.
	if rec := w.share("root", w.alicesAcct, account.ReceiverTeam, w.teamTwo.Meta.ID); rec.Code != http.StatusForbidden {
		t.Fatalf("the admin token shares a personal account: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := w.share("erin", w.teamAcct, account.ReceiverTeam, w.teamTwo.Meta.ID); rec.Code != http.StatusForbidden {
		t.Fatalf("a viewer's share: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := w.share("dave", w.teamAcct, account.ReceiverTeam, ids.New()); rec.Code != http.StatusNotFound {
		t.Fatalf("a share with a missing team: %d %s, want 404", rec.Code, rec.Body)
	}

	rec = w.share("dave", w.teamAcct, account.ReceiverTeam, w.teamTwo.Meta.ID)
	wantStatus(t, rec, http.StatusCreated)
	toTeam := decodeAccount(t, rec)
	if rec := w.share("bob", &toTeam, account.ReceiverUser, w.users["alice"]); rec.Code != http.StatusConflict || errorCode(t, rec) != "reshare_refused" {
		t.Fatalf("the receiving admin shares on: %d %s, want 409 reshare_refused", rec.Code, rec.Body)
	}

	rec = w.share("dave", w.teamAcct, account.ReceiverServiceAccount, w.sa.Meta.ID)
	wantStatus(t, rec, http.StatusCreated)
	toSA := decodeAccount(t, rec)
	if toSA.Meta.Owner != (meta.Owner{Kind: meta.OwnerProject, ID: w.projTwo.Meta.ID}) || toSA.Spec.Holder == nil || toSA.Spec.Holder.ID != w.sa.Meta.ID {
		t.Fatalf("service account share = %+v holder %+v, want its project's, held by it", toSA.Meta.Owner, toSA.Spec.Holder)
	}

	keyBody := `{"metadata":{"name":"on-a-share","owner":{"kind":"team","id":"` + w.teamTwo.Meta.ID + `"}},"spec":{"hostId":"` + w.host.Meta.ID + `","accountId":"` + toTeam.Meta.ID + `","valueFrom":{"kind":"env","env":"SHARE_TEST_KEY"}}}`
	if rec := w.send("root", http.MethodPost, "/host-keys", keyBody); rec.Code != http.StatusConflict || errorCode(t, rec) != "account_shared" {
		t.Fatalf("host key on a share: %d %s, want 409 account_shared", rec.Code, rec.Body)
	}
	if rec := w.send("alice", http.MethodPost, "/accounts", `{"metadata":{"name":"forged"},"spec":{"hostId":"`+w.host.Meta.ID+`","sourceAccountId":"`+w.teamAcct.Meta.ID+`"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a share made through POST /accounts: %d %s, want 400", rec.Code, rec.Body)
	}
}

// The owner lists shares with each receiver and what it drew; the receiver sees the share and the real account's keys read-only, never their secret.
func TestShares_ListAndReadOnlyKeys(t *testing.T) {
	const secret = "sk-share-0123456789abcdefghijkl"
	t.Setenv("SHARE_TEST_KEY", secret)
	w := newSharesWorld(t)
	rec := w.share("alice", w.alicesAcct, account.ReceiverTeam, w.teamTwo.Meta.ID)
	wantStatus(t, rec, http.StatusCreated)
	share := decodeAccount(t, rec)
	w.usage.events = []usagelog.Event{
		{RequestID: "r1", Status: 200, AccountID: w.alicesAcct.Meta.ID, ViaAccountID: share.Meta.ID},
		{RequestID: "r2", Status: 200, AccountID: w.alicesAcct.Meta.ID, ViaAccountID: share.Meta.ID},
		{RequestID: "r3", Status: 200, AccountID: w.alicesAcct.Meta.ID},
	}

	rec = w.send("alice", http.MethodGet, "/accounts/"+w.alicesAcct.Meta.Name+"/shares?since=7d", "")
	wantStatus(t, rec, http.StatusOK)
	var list AccountShares
	must(t, "decode shares", json.Unmarshal(rec.Body.Bytes(), &list))
	if len(list.Items) != 1 || list.Items[0].SharedAccount.ID != share.Meta.ID || list.Items[0].Receiver != (account.ShareParty{Kind: "team", ID: w.teamTwo.Meta.ID, Name: "two"}) {
		t.Fatalf("shares = %+v", list.Items)
	}
	if u := list.Items[0].Usage; u == nil || u.Requests != 2 {
		t.Fatalf("share usage = %+v, want the two requests through it", u)
	}
	if rec := w.send("bob", http.MethodGet, "/accounts/"+w.alicesAcct.Meta.ID+"/shares", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("the receiver lists the owner's shares: %d, want 404", rec.Code)
	}

	rec = w.send("bob", http.MethodGet, "/accounts/"+share.Meta.ID+"/keys", "")
	wantStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	if !strings.Contains(body, `"name":"alice-key"`) || !strings.Contains(body, `"readOnly":true`) || !strings.Contains(body, `"prefix":"sk-...ijkl"`) || strings.Contains(body, "SHARE_TEST_KEY") || strings.Contains(body, "sk-share") {
		t.Fatalf("the receiver's view of the shared keys = %s, want only the format prefix and the last 4", body)
	}
	// The owner keeps the start of the secret to recognise it by.
	rec = w.send("alice", http.MethodGet, "/accounts/"+w.alicesAcct.Meta.ID+"/keys", "")
	if !strings.Contains(rec.Body.String(), `"prefix":"sk-share"`) || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("the owner's view of the keys = %s", rec.Body)
	}
	if rec := w.send("bob", http.MethodGet, "/host-keys/"+w.aliceKey.Meta.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("the receiver reads the real key: %d, want 404", rec.Code)
	}
	if rec := w.send("bob", http.MethodGet, "/host-keys/by-id/"+w.aliceKey.Meta.ID+"/health", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state"`) {
		t.Fatalf("the receiver reads the shared key's health: %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := w.send("carol", http.MethodGet, "/host-keys/by-id/"+w.aliceKey.Meta.ID+"/health", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("a stranger reads the key's health: %d, want 404", rec.Code)
	}
}

// A share is deleted through the account delete, by the owner or the receiving scope's admin, answering which pools lose it; the real account's delete is blocked by its shares until detach deletes them.
func TestShares_Delete(t *testing.T) {
	w := newSharesWorld(t)
	ctx := t.Context()
	rec := w.share("dave", w.teamAcct, account.ReceiverTeam, w.teamTwo.Meta.ID)
	wantStatus(t, rec, http.StatusCreated)
	toTeam := decodeAccount(t, rec)
	rec = w.share("dave", w.teamAcct, account.ReceiverUser, w.users["bob"])
	wantStatus(t, rec, http.StatusCreated)
	toBob := decodeAccount(t, rec)
	two := toTeam.Meta.Owner
	p := &pool.Pool{Meta: meta.Metadata{ID: ids.New(), Name: "two-pool", Owner: two}, Spec: pool.Spec{HostID: w.host.Meta.ID, AccountIDs: []string{toTeam.Meta.ID}}}
	must(t, "pool", w.stores.Pool.Upsert(ctx, p))
	// The receivers' own rows on the shares, which the sharer may neither see nor edit: a binding holding only the team's share, and a key of bob's whose lane is his share.
	pol := &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: "two-cap", Owner: two}}
	must(t, "policy", w.stores.Policy.Upsert(ctx, pol))
	pb := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: ids.New(), Name: "two-cap-binding", Owner: two}, Spec: policybinding.Spec{PolicyID: pol.Meta.ID, AccountIDs: []string{toTeam.Meta.ID}}}
	must(t, "policy binding", w.stores.PolicyBinding.Upsert(ctx, pb))
	bobKey := &key.Key{Meta: meta.Metadata{ID: ids.New(), Name: "bob-on-share", Owner: meta.Owner{Kind: meta.OwnerUser, ID: w.users["bob"]}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: w.users["bob"]}, AccountID: toBob.Meta.ID, KeyHash: strings.Repeat("ab", 32)}}
	must(t, "key", w.stores.Key.Upsert(ctx, bobKey))

	body := decodeInUse(t, w.send("dave", http.MethodDelete, "/accounts/by-id/"+w.teamAcct.Meta.ID, ""))
	if len(body.Blockers) != 1 || body.Blockers[0].Kind != "account" || body.Blockers[0].Field != "spec.sourceAccountId" || !body.Blockers[0].Detachable {
		t.Fatalf("real account delete blockers = %+v, want its shares, detachable", body.Blockers)
	}

	rec = w.send("carol", http.MethodDelete, "/accounts/by-id/"+toTeam.Meta.ID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a stranger deletes a share: %d, want 404", rec.Code)
	}
	// Revoking always goes through, though the sharer may edit neither row: the binding left with no account goes and the pool loses the share, each listed (by name, as dave views team two).
	rec = w.send("dave", http.MethodDelete, "/accounts/by-id/"+toTeam.Meta.ID, "")
	wantStatus(t, rec, http.StatusOK)
	answer := rec.Body.String()
	for _, want := range []string{`"kind":"pool","field":"spec.accountIds","detachable":false,"count":1`, `"name":"two-pool"`, `"kind":"policy-binding","field":"spec.accountIds","detachable":false,"deleted":true,"count":1`, `"name":"two-cap-binding"`} {
		if !strings.Contains(answer, want) {
			t.Fatalf("share delete answer = %s, want %s", answer, want)
		}
	}
	if got, _ := w.stores.Pool.Get(ctx, p.Meta.ID); got == nil || len(got.Spec.AccountIDs) != 0 {
		t.Fatalf("pool after the share went = %+v", got)
	}
	if got, _ := w.stores.PolicyBinding.Get(ctx, pb.Meta.ID); got != nil {
		t.Fatalf("binding left with no account = %+v, want it gone", got)
	}

	rec = w.share("dave", w.teamAcct, account.ReceiverTeam, w.teamTwo.Meta.ID)
	wantStatus(t, rec, http.StatusCreated)
	again := decodeAccount(t, rec)
	p.Spec.AccountIDs, p.Meta.ResourceVersion = []string{again.Meta.ID}, ""
	must(t, "pool", w.stores.Pool.Upsert(ctx, p))
	rec = w.send("bob", http.MethodDelete, "/accounts/by-id/"+again.Meta.ID, "")
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"name":"two-pool"`) {
		t.Fatalf("the receiving admin's delete answer = %s, want its pool named", rec.Body)
	}

	// Detach revokes bob's share although a key of his runs in it: the key moves to his personal account and keeps working.
	det := decodeDetach(t, w.send("dave", http.MethodPost, "/accounts/by-id/"+w.teamAcct.Meta.ID+"/detach", ""))
	kinds := map[string]bool{}
	for _, g := range det.Detached {
		kinds[g.Kind+" "+g.Field] = true
	}
	if !kinds["account spec.sourceAccountId"] || !kinds["key spec.accountId"] || len(det.Blockers) != 0 {
		t.Fatalf("detach = %+v, want bob's share revoked, his key moved and nothing left", det)
	}
	if got, _ := w.stores.Account.Get(ctx, toBob.Meta.ID); got != nil {
		t.Fatalf("bob's share after detach = %+v", got)
	}
	moved, err := w.stores.Key.Get(ctx, bobKey.Meta.ID)
	must(t, "read key", err)
	lane, err := w.stores.Account.Get(ctx, moved.Spec.AccountID)
	must(t, "read lane", err)
	if lane == nil || lane.DerivedKind() != account.KindPersonal || lane.Spec.Holder.ID != w.users["bob"] {
		t.Fatalf("bob's key after the revoke runs in %+v, want his personal account", lane)
	}
	wantStatus(t, w.send("dave", http.MethodDelete, "/accounts/by-id/"+w.teamAcct.Meta.ID, ""), http.StatusNoContent)
}

// The sharer's revoke counts the receiver's rows it cannot see instead of naming them: a key of bob's whose lane was alice's share moves to his personal account, and alice learns only that one key moved.
func TestShares_RevokeCountsWhatTheSharerCannotSee(t *testing.T) {
	w := newSharesWorld(t)
	ctx := t.Context()
	rec := w.share("alice", w.alicesAcct, account.ReceiverUser, w.users["bob"])
	wantStatus(t, rec, http.StatusCreated)
	toBob := decodeAccount(t, rec)
	k := &key.Key{Meta: meta.Metadata{ID: ids.New(), Name: "bob-lane-key", Owner: meta.Owner{Kind: meta.OwnerUser, ID: w.users["bob"]}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: w.users["bob"]}, AccountID: toBob.Meta.ID, KeyHash: strings.Repeat("cd", 32)}}
	must(t, "key", w.stores.Key.Upsert(ctx, k))

	rec = w.send("alice", http.MethodDelete, "/accounts/by-id/"+toBob.Meta.ID, "")
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"kind":"key","field":"spec.accountId","detachable":false,"count":1,"hidden":1,"items":[]`) {
		t.Fatalf("revoke answer = %s, want the moved key counted, unnamed", rec.Body)
	}
	if moved, _ := w.stores.Key.Get(ctx, k.Meta.ID); moved == nil || moved.Spec.AccountID == toBob.Meta.ID || moved.Spec.AccountID == "" {
		t.Fatalf("bob's key after the revoke = %+v, want it in his own account", moved)
	}
}

// A share goes with its receiver: deleting the service account or the project it was shared with deletes it instead of blocking, and the answer lists the pools that lose it.
func TestShares_GoWithTheReceiver(t *testing.T) {
	w := newSharesWorld(t)
	ctx := t.Context()
	toSA := decodeAccount(t, w.share("alice", w.alicesAcct, account.ReceiverServiceAccount, w.sa.Meta.ID))
	toProject := decodeAccount(t, w.share("alice", w.alicesAcct, account.ReceiverProject, w.projTwo.Meta.ID))
	if toSA.Meta.ID == "" || toProject.Meta.ID == "" {
		t.Fatal("shares were not made")
	}
	p := &pool.Pool{Meta: meta.Metadata{ID: ids.New(), Name: "two-app-pool", Owner: toSA.Meta.Owner}, Spec: pool.Spec{HostID: w.host.Meta.ID, AccountIDs: []string{toSA.Meta.ID}}}
	must(t, "pool", w.stores.Pool.Upsert(ctx, p))

	rec := w.send("root", http.MethodDelete, "/service-accounts/by-id/"+w.sa.Meta.ID, "")
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"name":"two-app-pool"`) || !strings.Contains(rec.Body.String(), `"field":"spec.accountIds"`) {
		t.Fatalf("service account delete answer = %s, want the pool that lost its share", rec.Body)
	}
	if got, _ := w.stores.Account.Get(ctx, toSA.Meta.ID); got != nil {
		t.Fatalf("the service account's share after its delete = %+v", got)
	}

	must(t, "drop pool", w.stores.Pool.Delete(ctx, p.Meta.ID))
	rec = w.send("root", http.MethodDelete, "/projects/by-id/"+w.projTwo.Meta.ID, "")
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("project delete with a share = %d %s, want it to go through", rec.Code, rec.Body)
	}
	if got, _ := w.stores.Account.Get(ctx, toProject.Meta.ID); got != nil {
		t.Fatalf("the project's share after its delete = %+v", got)
	}
}
