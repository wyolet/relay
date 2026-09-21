package inference

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
)

func errCode(t *testing.T, body []byte) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return payload.Error.Code
}

// D77: a key whose policy an operator switched off answers 403
// policy_disabled: the key is still valid, its policy is not. It used to be
// dropped from the snapshot and rejected as an unknown key.
func TestPrincipal_KeyOnDisabledPolicyIsForbidden(t *testing.T) {
	f := newPrincipalFixture()
	off := false
	f.keyPol.Spec.Enabled = &off

	k := saKey(f, "sk-disabled-pol")
	k.Spec.PolicyID = f.keyPol.Meta.ID
	w := f.stack(t, k).do("sk-disabled-pol")

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", w.Code, w.Body.String())
	}
	if got := errCode(t, w.Body.Bytes()); got != "policy_disabled" {
		t.Fatalf("code = %q, want policy_disabled", got)
	}
}

// D77: a binding whose policy is disabled is an answer, not a miss:
// resolution stops there instead of falling through to the next binding.
func TestPrincipal_DisabledBindingDoesNotFallThrough(t *testing.T) {
	f := newPrincipalFixture()
	f.sa.Spec.PolicyID = ""
	off := false
	f.keyPol.Spec.Enabled = &off

	pb := func(name, policyID string, priority int) *policybinding.PolicyBinding {
		b := &policybinding.PolicyBinding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name},
			Spec: policybinding.Spec{
				ProjectID: f.project.Meta.ID, PolicyID: policyID, Priority: &priority,
				Subjects: []rolebinding.Subject{{Kind: rolebinding.SubjectGroup, Name: "system:authenticated"}},
			},
		}
		b.StampOwner()
		return b
	}
	// The disabled policy binds first; the broader one would win on a
	// fall-through.
	f.bindings = []*policybinding.PolicyBinding{
		pb("first-disabled", f.keyPol.Meta.ID, 10),
		pb("second-live", f.boundPol.Meta.ID, 500),
	}

	w := f.stack(t, saKey(f, "sk-binding")).do("sk-binding")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", w.Code, w.Body.String())
	}
	if got := errCode(t, w.Body.Bytes()); got != "policy_disabled" {
		t.Fatalf("code = %q, want policy_disabled", got)
	}
}

// D77 on the long-lived path: a WebSocket connection re-resolves its
// principal per frame, so a policy switched off mid-connection has to answer
// policy_disabled on the next frame rather than falling through to the
// account's or the project's broader grant.
func TestFramePrincipal_KeyOnDisabledPolicyResolvesTheDisabledRow(t *testing.T) {
	f := newPrincipalFixture()
	off := false
	f.keyPol.Spec.Enabled = &off

	k := saKey(f, "sk-ws-disabled")
	k.Spec.PolicyID = f.keyPol.Meta.ID
	st := f.stack(t, k)
	snap := st.cat.Current()

	conn := &Principal{CredentialKind: CredentialKey, KeyHash: sha("sk-ws-disabled")}
	frame := framePrincipal(conn, snap)
	if frame.Policy == nil {
		t.Fatal("frame policy is nil: the frame falls through to the account or the project")
	}
	if frame.Policy.Meta.ID != f.keyPol.Meta.ID || frame.Policy.IsEnabled() {
		t.Fatalf("frame policy = %+v, want the disabled row the key names", frame.Policy.Meta)
	}

	w := httptest.NewRecorder()
	if resolvePolicy(w, snap, frame) {
		t.Fatal("resolvePolicy admitted a frame on a disabled policy")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if got := errCode(t, w.Body.Bytes()); got != "policy_disabled" {
		t.Fatalf("code = %q, want policy_disabled", got)
	}
}

// D77: a service account's own disabled policy override answers the
// same way rather than dropping the account from the snapshot.
func TestPrincipal_ServiceAccountOnDisabledPolicyIsForbidden(t *testing.T) {
	f := newPrincipalFixture()
	off := false
	f.saPol.Spec.Enabled = &off

	w := f.stack(t, saKey(f, "sk-sa-pol")).do("sk-sa-pol")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", w.Code, w.Body.String())
	}
	if got := errCode(t, w.Body.Bytes()); got != "policy_disabled" {
		t.Fatalf("code = %q, want policy_disabled", got)
	}
}
