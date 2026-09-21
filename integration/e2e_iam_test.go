//go:build integration

// e2e_iam_test.go walks six operator scenarios in the order they happen,
// each one across both planes. The neighbouring files pin the individual
// promises; what is checked here is the order and the seams between the
// steps. Upstream traffic uses the in-process mock the token fixture wires,
// and usage is read from an in-memory collector rather than a sink.
package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-migrate/migrate/v4"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/lifecycle"
)

func TestOperatorWalk(t *testing.T) {
	t.Run("UnresolvedHostKeyDoesNotBlockTheControlPlane", walkUnresolvedHostKey)
	t.Run("MintedTokenSpendsAndItsUsageCarriesTheProject", walkTokenUsageAttribution)
	t.Run("TeamAdminCannotRebindTheirOwnBindingToTheAdminRole", walkRoleBindingUpdateEscalation)
	t.Run("LegacyKeysListForTheirOwnerAfterTheUpgrade", walkLegacyKeysAfterUpgrade)
	t.Run("PolicyBindingPriorityZeroBeatsAnUnsetPriority", walkPolicyBindingPriority)
	t.Run("WebSocketFramesFollowAPolicyBindingChange", walkWebSocketFollowsRebinding)
}

// The repair path for an unreadable credential runs through a full rebuild,
// and the traffic the working credentials serve must not stop while the
// operator fixes the reference. What the row itself still answers on the
// control plane is hostkey_test.go's.
func walkUnresolvedHostKey(t *testing.T) {
	f := newTokenFixture(t)
	broken := f.seedUnresolvableHostKey(t)

	if code, raw := f.adminDo(http.MethodPost, "/api/reload", ""); code != http.StatusOK {
		t.Fatalf("POST /api/reload with an unresolvable host key = %d: %s", code, raw)
	}
	if _, ok := f.cat.Current().HostKey(broken.Meta.ID); ok {
		t.Error("the rebuilt snapshot kept the unresolvable host key")
	}

	us := f.login(t, f.username, f.password)
	code, token, raw := f.mint(us)
	if code != http.StatusOK {
		t.Fatalf("mint = %d: %s", code, raw)
	}
	if got, reason := f.chat(token, "test-model"); got != http.StatusOK {
		t.Fatalf("inference on the working credential = %d (%s)", got, reason)
	}
}

// A token is scoped to a project so the usage it produces can be billed to
// one: the record has to carry the project, its team and the principal.
func walkTokenUsageAttribution(t *testing.T) {
	f := newTokenFixture(t)
	events := captureUsage(t, f.stack)
	project := f.projectRow(t)

	us := f.login(t, f.username, f.password)
	code, token, raw := f.mint(us)
	if code != http.StatusOK {
		t.Fatalf("mint = %d: %s", code, raw)
	}
	if got, reason := f.chat(token, "test-model"); got != http.StatusOK {
		t.Fatalf("chat with a minted token = %d (%s)", got, reason)
	}

	ev := events.wait(t, 5*time.Second)
	if ev.ProjectID != project.Metadata.ID {
		t.Errorf("usage project id = %q, want the project the token names (%s)", ev.ProjectID, project.Metadata.ID)
	}
	if ev.TeamID != project.Spec.TeamID {
		t.Errorf("usage team id = %q, want the project's team (%s)", ev.TeamID, project.Spec.TeamID)
	}
	if ev.PrincipalKind != "user" || ev.PrincipalID != f.userID {
		t.Errorf("usage principal = %s/%s, want user/%s", ev.PrincipalKind, ev.PrincipalID, f.userID)
	}
}

// Escalation prevention has to hold on the update path too: a binding the
// team admin may own must not become a wider one under a PUT.
func walkRoleBindingUpdateEscalation(t *testing.T) {
	st := newStackAuthz(t, "rbac")
	roles := seedBuiltinRoles(t, st)
	teamA := st.mkTeam(t, "alpha")
	adminID := st.seedLogin(t, "tadmin", "pw-tadmin")
	devID := st.seedLogin(t, "dev", "pw-dev")
	st.bindRole(t, "alpha-admins", roles["team-admin"].Meta.ID, adminID, "team", teamA)
	if err := st.cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	admin := st.login(t, "tadmin", "pw-tadmin")
	bindingBody := func(name, roleID, subjects string) string {
		return `{"metadata":{"name":"` + name + `","owner":{"kind":"team","id":"` + teamA + `"}},` +
			`"spec":{"roleId":"` + roleID + `","scope":{"kind":"team","id":"` + teamA + `"},` +
			`"subjects":[` + subjects + `]}}`
	}
	devSubject := `{"kind":"user","id":"` + devID + `"}`

	// The role the binder holds is bindable at their own scope.
	if code, raw := admin.do(http.MethodPost, "/api/role-bindings",
		bindingBody("alpha-more-admins", roles["team-admin"].Meta.ID, devSubject)); code != http.StatusCreated {
		t.Fatalf("team-admin binding their own role at their own team = %d, want 201: %s", code, raw)
	}
	code, raw := admin.do(http.MethodPost, "/api/role-bindings",
		bindingBody("alpha-devs", roles["developer"].Meta.ID, devSubject))
	if code != http.StatusCreated {
		t.Fatalf("team-admin binding the developer role = %d, want 201: %s", code, raw)
	}
	id := roleBindingRow(t, raw).Metadata.ID

	// A benign edit of the same row goes through, so the refusal below is
	// the escalation and not the update itself.
	withAdminSubject := bindingBody("alpha-devs", roles["developer"].Meta.ID,
		devSubject+`,{"kind":"user","id":"`+adminID+`"}`)
	if code, raw := admin.do(http.MethodPut, "/api/role-bindings/by-id/"+id, withAdminSubject); code != http.StatusOK {
		t.Fatalf("team-admin adding a subject to their own binding = %d, want 200: %s", code, raw)
	}

	widened := bindingBody("alpha-devs", roles["admin"].Meta.ID, devSubject)
	code, raw = admin.do(http.MethodPut, "/api/role-bindings/by-id/"+id, widened)
	if code != http.StatusForbidden {
		t.Fatalf("team-admin widening their binding to the admin role = %d, want 403: %s", code, raw)
	}
	if !strings.Contains(string(raw), "would grant") {
		t.Errorf("refusal body = %s, want it to name the permission the binder does not hold", raw)
	}

	code, raw = st.adminDo(http.MethodGet, "/api/role-bindings/alpha-devs", "")
	if code != http.StatusOK {
		t.Fatalf("GET role binding = %d: %s", code, raw)
	}
	if got := roleBindingRow(t, raw).Spec.RoleID; got != roles["developer"].Meta.ID {
		t.Errorf("stored role after the refused update = %q, want the developer role %q", got, roles["developer"].Meta.ID)
	}
}

// An upgraded deployment carries keys minted before tenancy and has no role
// bindings yet. The backfill gives each key its owner's uuid, and a caller
// with no binding still sees the rows that are personally theirs — only
// those.
func walkLegacyKeysAfterUpgrade(t *testing.T) {
	st := newStackAuthz(t, "rbac")
	ctx := context.Background()
	mineID := st.seedLogin(t, "sluggy", "pw-sluggy")
	if err := st.users.Upsert(ctx, &user.User{ID: ids.New(), Username: "otherguy"}); err != nil {
		t.Fatalf("seed the other user: %v", err)
	}

	m := migrator(t)
	// A failure between the two steps below would leave the shared schema
	// behind head for everything that runs after this test.
	t.Cleanup(func() { _ = m.Up() })
	if err := m.Migrate(25); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate down to 25: %v", err)
	}
	insertLegacySlugKey(t, "mine", "sluggy")
	insertLegacySlugKey(t, "theirs", "otherguy")
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate back up to head: %v", err)
	}
	if err := st.cat.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	mine := st.login(t, "sluggy", "pw-sluggy")
	code, raw := mine.do(http.MethodGet, "/api/keys", "")
	if code != http.StatusOK {
		t.Fatalf("key list for a caller with no bindings = %d, want 200: %s", code, raw)
	}
	var list struct {
		Items []keyRow `json:"items"`
		Total int      `json:"total"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode key list: %v", err)
	}
	if list.Total != 1 || len(list.Items) != 1 {
		t.Fatalf("key list = %d rows, want the caller's own legacy key only: %s", list.Total, raw)
	}
	if got := list.Items[0]; got.Metadata.Name != "mine" || got.Metadata.Owner.ID != mineID {
		t.Errorf("listed key = %q owned by %q, want \"mine\" owned by %s",
			got.Metadata.Name, got.Metadata.Owner.ID, mineID)
	}
}

// Priority orders the bindings of one project and an unset priority is the
// documented default, so an explicit zero has to win over a binding that
// declares none, whichever way the names sort.
func walkPolicyBindingPriority(t *testing.T) {
	f := newTokenFixture(t)
	winner := f.seedPolicyForModel(t, "priority-winner", "second-model")
	project := f.projectRow(t)

	// The fixture's binding declares no priority and sorts first by name;
	// this one declares zero, so only priority can put it in front.
	body := `{"metadata":{"name":"zzz-explicit-zero"},"spec":{"projectId":"` + project.Metadata.ID + `",` +
		`"policyId":"` + winner + `","priority":0,` +
		`"subjects":[{"kind":"group","name":"system:authenticated"}]}}`
	if code, raw := f.adminDo(http.MethodPost, "/api/policy-bindings", body); code != http.StatusCreated {
		t.Fatalf("POST /api/policy-bindings = %d: %s", code, raw)
	}
	f.waitForTopPolicyBinding(t, project.Metadata.ID, winner)

	us := f.login(t, f.username, f.password)
	code, token, raw := f.mint(us)
	if code != http.StatusOK {
		t.Fatalf("mint = %d: %s", code, raw)
	}
	if got, reason := f.chat(token, "second-model"); got != http.StatusOK {
		t.Fatalf("model granted by the priority-0 binding = %d (%s), want 200", got, reason)
	}
	got, reason := f.chat(token, "test-model")
	if got != http.StatusForbidden || reason != "model_not_allowed" {
		t.Fatalf("model granted only by the unset-priority binding = %d (%s), want 403 model_not_allowed", got, reason)
	}
}

// A connection lives for hours and the bindings behind it do not, so a
// frame sent after the project was rebound is answered by the new policy
// without the client reconnecting. The rebinding propagates over NOTIFY,
// the way it reaches a live pod.
func walkWebSocketFollowsRebinding(t *testing.T) {
	f := newTokenFixture(t)
	rebound := f.seedPolicyForModel(t, "rebound-policy", "second-model")
	project := f.projectRow(t)

	us := f.login(t, f.username, f.password)
	code, token, raw := f.mint(us)
	if code != http.StatusOK {
		t.Fatalf("mint = %d: %s", code, raw)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.inference.URL, "http")+"/v1/ws",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	if status, reason := wsGenerate(ctx, t, conn, "first", "test-model"); status != http.StatusOK {
		t.Fatalf("first frame under the original binding = %d (%s), want 200", status, reason)
	}

	code, raw = f.adminDo(http.MethodGet, "/api/policy-bindings/token-project-everyone", "")
	if code != http.StatusOK {
		t.Fatalf("GET policy binding = %d: %s", code, raw)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode policy binding: %v", err)
	}
	delete(row, "$schema")
	spec, _ := row["spec"].(map[string]any)
	spec["policyId"] = rebound
	body, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("encode policy binding: %v", err)
	}
	id, _ := row["metadata"].(map[string]any)["id"].(string)
	if code, raw := f.adminDo(http.MethodPut, "/api/policy-bindings/by-id/"+id, string(body)); code != http.StatusOK {
		t.Fatalf("PUT policy binding = %d: %s", code, raw)
	}
	f.waitForTopPolicyBinding(t, project.Metadata.ID, rebound)

	if status, reason := wsGenerate(ctx, t, conn, "second", "second-model"); status != http.StatusOK {
		t.Fatalf("frame for the rebound policy's model = %d (%s), want 200", status, reason)
	}
	status, reason := wsGenerate(ctx, t, conn, "third", "test-model")
	if status != http.StatusForbidden || reason != "model_not_allowed" {
		t.Fatalf("frame for the model the connection started on = %d (%s), want 403 model_not_allowed", status, reason)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

// projectRow reads the project the fixture's token is minted against.
func (f *tokenFixture) projectRow(t *testing.T) tenancyRow {
	t.Helper()
	code, raw := f.adminDo(http.MethodGet, "/api/projects/"+f.project, "")
	if code != http.StatusOK {
		t.Fatalf("GET project = %d: %s", code, raw)
	}
	var row tenancyRow
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	return row
}

type roleBindingBody struct {
	Metadata struct {
		ID string `json:"id"`
	} `json:"metadata"`
	Spec struct {
		RoleID string `json:"roleId"`
	} `json:"spec"`
}

func roleBindingRow(t *testing.T, raw []byte) roleBindingBody {
	t.Helper()
	var row roleBindingBody
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode role binding: %v", err)
	}
	return row
}

// usageCapture collects the usage events the post-flight goroutine produces.
type usageCapture struct {
	mu     sync.Mutex
	events []usagelog.Event
}

// captureUsage registers the usage producer plus a collector holding the
// events in memory, so a scenario can read what a request was billed to.
// Registration happens before any traffic, as at boot.
func captureUsage(t *testing.T, s *stack) *usageCapture {
	t.Helper()
	c := &usageCapture{}
	s.lifecycle.RegisterHook(usagelog.NewUsageHook(nil, ""))
	s.lifecycle.RegisterCollector(c)
	return c
}

func (c *usageCapture) Collect(lc *lifecycle.Context) {
	v, ok := lc.Collected(usagelog.Namespace)
	if !ok {
		return
	}
	ev, ok := v.(*usagelog.Event)
	if !ok || ev == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, *ev)
}

// wait blocks until an event lands: post-flight is detached, so the
// caller's 200 does not mean the record exists yet.
func (c *usageCapture) wait(t *testing.T, within time.Duration) usagelog.Event {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		c.mu.Lock()
		var first usagelog.Event
		found := len(c.events) > 0
		if found {
			first = c.events[0]
		}
		c.mu.Unlock()
		if found {
			return first
		}
		if time.Now().After(deadline) {
			t.Fatal("no usage event was recorded for the request")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// seedPolicyForModel adds a model on the happy path's host and a policy
// granting it, and returns the policy id. The model is new, so a policy
// naming it is distinguishable from the one the fixture wired.
func (f *tokenFixture) seedPolicyForModel(t *testing.T, policyName, modelName string) string {
	t.Helper()
	ctx := context.Background()
	snap := f.cat.Current()
	hst, ok := snap.HostByName("test-host")
	if !ok {
		t.Fatal("the happy path host is missing")
	}
	base, ok := snap.PolicyByName("test-policy")
	if !ok {
		t.Fatal("the happy path policy is missing")
	}
	prov, ok := snap.ProviderByName("test-provider")
	if !ok {
		t.Fatal("the happy path provider is missing")
	}

	mdl := &model.Model{
		Meta: meta.Metadata{ID: ids.New(), Name: modelName, Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: modelName}}, Pointer: modelName},
	}
	mustUpsert(t, f.stores.Model.Upsert(ctx, mdl), "model")
	bnd := &binding.Binding{
		Meta: meta.Metadata{ID: ids.New(), Name: modelName + "-on-test-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: mdl.Meta.ID, HostID: hst.Meta.ID, Adapter: adapters.OpenAI},
	}
	mustUpsert(t, f.stores.Binding.Upsert(ctx, bnd), "binding")
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: ids.New(), Name: policyName, Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: policy.Spec{
			ModelIDs:     []string{mdl.Meta.ID},
			HostKeyIDs:   base.Spec.HostKeyIDs,
			KeySelection: policy.KeySelectionPrioritized,
		},
	}
	mustUpsert(t, f.stores.Policy.Upsert(ctx, pol), "policy")
	f.waitForSnapshot(t, func(s *appcatalog.Snapshot) bool {
		_, ok := s.Policy(pol.Meta.ID)
		return ok
	}, "the seeded policy never reached the snapshot")
	return pol.Meta.ID
}

// waitForTopPolicyBinding blocks until the project's highest-priority
// binding names policyID.
func (f *tokenFixture) waitForTopPolicyBinding(t *testing.T, projectID, policyID string) {
	t.Helper()
	f.waitForSnapshot(t, func(s *appcatalog.Snapshot) bool {
		bindings := s.PolicyBindingsForProject(projectID)
		return len(bindings) > 0 && bindings[0].Spec.PolicyID == policyID
	}, "the policy binding change never reached the snapshot")
}

// waitForSnapshot polls the live snapshot until cond holds. Writes reach it
// over PG NOTIFY, which is the path a running deployment uses; forcing a
// rebuild in-process would skip it.
func (s *stack) waitForSnapshot(t *testing.T, cond func(*appcatalog.Snapshot) bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if cond(s.cat.Current()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// insertLegacySlugKey writes a pre-tenancy key row: owned by a username
// rather than a user id, with no principal at all.
func insertLegacySlugKey(t *testing.T, name, username string) {
	t.Helper()
	hash := sha256Hex(name)
	if _, err := testPool(t).Exec(context.Background(),
		`INSERT INTO relay_keys (id, name, display_name, key_hash, metadata, spec)
		 VALUES ($1, $2, '', $3, $4::jsonb, $5::jsonb)`,
		ids.New(), name, hash,
		`{"owner":{"kind":"user","id":"`+username+`"}}`,
		`{"keyHash":"`+hash+`","prefix":"sk-wr-`+name+`"}`); err != nil {
		t.Fatalf("insert legacy key %q: %v", name, err)
	}
}

// wsGenerate sends one canonical frame and returns the status its start
// frame carried plus the error code its body names, draining the frames for
// that id.
func wsGenerate(ctx context.Context, t *testing.T, conn *websocket.Conn, id, modelName string) (int, string) {
	t.Helper()
	frame, err := json.Marshal(map[string]any{
		"id":      id,
		"payload": json.RawMessage(`{"model":"` + modelName + `","input":"hi"}`),
	})
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	status, data := 0, ""
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		var out struct {
			ID     string `json:"id"`
			Event  string `json:"event"`
			Status int    `json:"status"`
			Data   string `json:"data"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		if out.ID != id {
			continue
		}
		if out.Status != 0 {
			status = out.Status
		}
		data += out.Data
		if out.Event == "end" {
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal([]byte(data), &body)
			return status, body.Error.Code
		}
	}
}
