package inference

import (
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
)

// A WebSocket admits once and then serves frames for hours; Recheck is what
// every frame runs so a revocation lands without a reconnect.
func TestPrincipalRecheckOnKeyRevocation(t *testing.T) {
	f := newPrincipalFixture()
	k := saKey(f, "sk-wr-live")
	st := f.stack(t, k)
	if w := st.do("sk-wr-live"); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	p := st.seen
	now := time.Now()
	if err := p.Recheck(st.cat.Current(), now); err != nil {
		t.Fatalf("Recheck on a live key: %v", err)
	}

	revoked := *k
	revoked.Spec.RevokedAt = &now
	if err := st.cat.ApplyKeyUpsert(&revoked); err != nil {
		t.Fatalf("apply key: %v", err)
	}
	if err := p.Recheck(st.cat.Current(), now); err == nil {
		t.Fatal("Recheck accepted a revoked key")
	}
}

func TestPrincipalRecheckOnKeyDeletion(t *testing.T) {
	f := newPrincipalFixture()
	k := saKey(f, "sk-wr-live")
	st := f.stack(t, k)
	st.do("sk-wr-live")
	p := st.seen

	if err := st.cat.ApplyKeyDelete(k.Meta.ID); err != nil {
		t.Fatalf("delete key: %v", err)
	}
	if err := p.Recheck(st.cat.Current(), time.Now()); err == nil {
		t.Fatal("Recheck accepted a deleted key")
	}
}

func TestPrincipalRecheckOnTokenExpiryAndVersionBump(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{
		boundTo(f, "bind-all", 10, f.boundPol.Meta.ID, "group:system:authenticated"),
	}
	st := f.stack(t)
	if w := st.do(f.mint(t, nil)); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	p := st.seen
	now := time.Now()
	if err := p.Recheck(st.cat.Current(), now); err != nil {
		t.Fatalf("Recheck on a live token: %v", err)
	}
	if err := p.Recheck(st.cat.Current(), time.Unix(p.TokenExp+1, 0)); err == nil {
		t.Fatal("Recheck accepted an expired token")
	}

	// A revoke-all bumps the user's version; the frame must stop.
	f.versions[f.user] = 2
	if err := st.cat.ReloadTokenVersions(t.Context()); err != nil {
		t.Fatalf("reload versions: %v", err)
	}
	if err := p.Recheck(st.cat.Current(), now); err == nil {
		t.Fatal("Recheck accepted a token whose version was bumped")
	}
}

func TestPrincipalRecheckOnKeyExpiryAndGrace(t *testing.T) {
	f := newPrincipalFixture()
	k := saKey(f, "sk-wr-live")
	st := f.stack(t, k)
	st.do("sk-wr-live")
	p := st.seen

	past := time.Now().Add(-time.Hour)
	expired := *k
	expired.Meta = meta.Metadata{ID: k.Meta.ID, Name: k.Meta.Name, Owner: k.Meta.Owner}
	expired.Spec.ExpiresAt = &past
	if err := st.cat.ApplyKeyUpsert(&expired); err != nil {
		t.Fatalf("apply key: %v", err)
	}
	if err := p.Recheck(st.cat.Current(), time.Now()); err == nil {
		t.Fatal("Recheck accepted an expired key")
	}
}

// A key principal's Recheck reads the hash it presented, so a rotated-away
// bearer stops working once its grace window closes.
func TestPrincipalRecheckAfterGraceCloses(t *testing.T) {
	f := newPrincipalFixture()
	k := saKey(f, "sk-wr-old")
	until := time.Now().Add(time.Minute)
	k.Spec.PreviousKeyHash = k.Spec.KeyHash
	k.Spec.KeyHash = sha("sk-wr-new")
	k.Spec.GraceUntil = &until
	st := f.stack(t, k)
	if w := st.do("sk-wr-old"); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	p := st.seen
	if err := p.Recheck(st.cat.Current(), time.Now()); err != nil {
		t.Fatalf("Recheck inside the grace window: %v", err)
	}
	if err := p.Recheck(st.cat.Current(), until.Add(time.Second)); err == nil {
		t.Fatal("Recheck accepted a rotated-away bearer past its grace window")
	}
}
