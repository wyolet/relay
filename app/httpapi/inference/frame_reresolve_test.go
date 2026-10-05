package inference

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
)

// removeMember empties a group the way the operator's "remove member" does: the row stays.
func removeMember(t *testing.T, st *principalStack, g *group.Group) {
	t.Helper()
	emptied := *g
	emptied.Spec = group.Spec{MemberIDs: nil}
	if err := st.cat.ApplyGroupUpsert(&emptied); err != nil {
		t.Fatalf("empty group: %v", err)
	}
}

// A frame resolves the service account's current policy override, as a fresh HTTP request on the same key does.
func TestFramePrincipal_FollowsServiceAccountOverride(t *testing.T) {
	f := newPrincipalFixture()
	st := f.stack(t, saKey(f, "sk-wr-live"))
	if w := st.do("sk-wr-live"); w.Code != http.StatusOK {
		t.Fatalf("upgrade: %d %s", w.Code, w.Body)
	}
	upgrade := st.seen
	if upgrade.PolicyID() != f.saPol.Meta.ID {
		t.Fatalf("upgrade policy = %s, want sa-pol", upgrade.PolicyID())
	}

	moved := *f.sa
	moved.Spec.PolicyID = f.keyPol.Meta.ID
	if err := st.cat.ApplyServiceAccountUpsert(&moved); err != nil {
		t.Fatalf("re-point sa: %v", err)
	}
	snap := st.cat.Current()
	if err := upgrade.Recheck(snap, time.Now()); err != nil {
		t.Fatalf("Recheck: %v", err)
	}
	frame := framePrincipal(upgrade, snap)
	if !resolvePolicy(httptest.NewRecorder(), snap, frame) || frame.PolicyID() != f.keyPol.Meta.ID {
		t.Fatalf("frame policy = %s, want the new override key-pol", frame.PolicyID())
	}
	if upgrade.PolicyID() != f.saPol.Meta.ID {
		t.Error("the shared principal was mutated by a frame")
	}
}

// A personal key whose project-owned policy was cleared and whose user left the group stops matching the group binding on the next frame.
func TestFramePrincipal_DropsRemovedGroupAndProject(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{
		boundTo(f, "bind-ds", 10, f.boundPol.Meta.ID, "group:data-science"),
	}
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "alice", Owner: meta.Owner{Kind: meta.OwnerUser, ID: f.user}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalUser, ID: f.user},
			PolicyID:  f.keyPol.Meta.ID,
			KeyHash:   sha("sk-wr-personal"),
		},
	}
	st := f.stack(t, k)
	if w := st.do("sk-wr-personal"); w.Code != http.StatusOK {
		t.Fatalf("upgrade: %d %s", w.Code, w.Body)
	}
	upgrade := st.seen
	if upgrade.ProjectID != f.project.Meta.ID || !hasSubject(upgrade.Subjects, "group:data-science") {
		t.Fatalf("upgrade principal: project=%s subjects=%v", upgrade.ProjectID, upgrade.Subjects)
	}

	unbound := *k
	unbound.Spec.PolicyID = ""
	if err := st.cat.ApplyKeyUpsert(&unbound); err != nil {
		t.Fatalf("unbind key: %v", err)
	}
	removeMember(t, st, f.group)
	snap := st.cat.Current()
	if err := upgrade.Recheck(snap, time.Now()); err != nil {
		t.Fatalf("Recheck: %v", err)
	}
	frame := framePrincipal(upgrade, snap)
	resolvePolicy(httptest.NewRecorder(), snap, frame)
	if hasSubject(frame.Subjects, "group:data-science") || frame.ProjectID != "" || frame.PolicyID() == f.boundPol.Meta.ID {
		t.Fatalf("frame kept stale state: subjects=%v project=%q policy=%s", frame.Subjects, frame.ProjectID, frame.PolicyID())
	}
}

// A token frame drops a local group the user left, as a fresh request with the same token does.
func TestFramePrincipal_TokenDropsRemovedLocalGroup(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{
		boundTo(f, "bind-ds", 10, f.boundPol.Meta.ID, "group:data-science"),
	}
	st := f.stack(t)
	tok := f.mint(t, nil)
	if w := st.do(tok); w.Code != http.StatusOK || st.seen.PolicyID() != f.boundPol.Meta.ID {
		t.Fatalf("upgrade: status=%d policy=%s", w.Code, st.seen.PolicyID())
	}
	upgrade := st.seen

	removeMember(t, st, f.group)
	snap := st.cat.Current()
	if err := upgrade.Recheck(snap, time.Now()); err != nil {
		t.Fatalf("Recheck: %v", err)
	}
	frame := framePrincipal(upgrade, snap)
	if hasSubject(frame.Subjects, "group:data-science") {
		t.Fatalf("frame subjects = %v, still carry the removed group", frame.Subjects)
	}
	w := httptest.NewRecorder()
	if resolvePolicy(w, snap, frame) || w.Code != http.StatusForbidden {
		t.Fatalf("frame still resolves a policy (%s) after the membership was removed", frame.PolicyID())
	}
}

// Key-level permissions withdrawn on the key row apply to the next frame, and a proxy frame is refused without passthrough.
func TestFramePrincipal_RederivesKeyFlags(t *testing.T) {
	f := newPrincipalFixture()
	k := saKey(f, "sk-wr-live")
	k.Spec.PassthroughAllowed = true
	k.Spec.PayloadLoggingEnabled = true
	st := f.stack(t, k)
	if w := st.do("sk-wr-live"); w.Code != http.StatusOK {
		t.Fatalf("upgrade: %d %s", w.Code, w.Body)
	}
	upgrade := st.seen
	if !upgrade.PassthroughAllowed || !upgrade.PayloadLogging {
		t.Fatalf("upgrade flags not set: %+v", upgrade)
	}

	flipped := *k
	flipped.Spec.PassthroughAllowed = false
	flipped.Spec.PayloadLoggingEnabled = false
	if err := st.cat.ApplyKeyUpsert(&flipped); err != nil {
		t.Fatalf("flip: %v", err)
	}
	snap := st.cat.Current()
	if err := upgrade.Recheck(snap, time.Now()); err != nil {
		t.Fatalf("Recheck: %v", err)
	}
	frame := framePrincipal(upgrade, snap)
	if frame.PassthroughAllowed || frame.PayloadLogging {
		t.Fatalf("frame kept upgrade-time flags: passthrough=%v payloadLogging=%v", frame.PassthroughAllowed, frame.PayloadLogging)
	}

	w := httptest.NewRecorder()
	if authorizePrincipal(w, snap, frame, ModeProxyAuthed) {
		t.Fatal("a proxy frame was admitted after passthrough was withdrawn")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if !authorizePrincipal(httptest.NewRecorder(), snap, framePrincipal(upgrade, snap), ModeNormal) {
		t.Error("a normal frame was refused")
	}
}
