package main

import (
	"slices"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/secret"
)

func TestOAuthRenewals(t *testing.T) {
	key := func(id string, kind hostkey.ValueKind, resolved string, revoked bool) *hostkey.HostKey {
		k := &hostkey.HostKey{Meta: meta.Metadata{ID: id}, Spec: hostkey.Spec{ValueFrom: hostkey.ValueFrom{Kind: kind, Provider: "service-account"}}, Resolved: resolved}
		if revoked {
			k.Status.Credential = &hostkey.CredentialStatus{State: hostkey.CredentialRevoked}
		}
		return k
	}
	listed := []*hostkey.HostKey{
		key("same", hostkey.ValueKindOAuth, "tok-1", false),
		key("reminted", hostkey.ValueKindOAuth, "tok-2", false),
		key("revoked-reminted", hostkey.ValueKindOAuth, "tok-2", true),
		key("unpublished", hostkey.ValueKindOAuth, "tok-1", false),
		key("stored", hostkey.ValueKindStored, "sk-2", false),
	}
	published := map[string]*hostkey.HostKey{
		"same":             key("same", hostkey.ValueKindOAuth, "tok-1", false),
		"reminted":         key("reminted", hostkey.ValueKindOAuth, "", false),
		"revoked-reminted": key("revoked-reminted", hostkey.ValueKindOAuth, "tok-1", true),
		"stored":           key("stored", hostkey.ValueKindStored, "sk-1", false),
	}

	refs, changed := oauthRenewals(listed, func(id string) *hostkey.HostKey { return published[id] })

	if want := []string{"reminted", "revoked-reminted"}; !slices.Equal(changed, want) {
		t.Fatalf("announced %v, want %v", changed, want)
	}
	var ids []string
	for _, r := range refs {
		if r.Kind != secret.KindOAuth || r.Provider != "service-account" {
			t.Fatalf("ref %+v", r)
		}
		ids = append(ids, r.ID)
	}
	if want := []string{"same", "reminted", "unpublished"}; !slices.Equal(ids, want) {
		t.Fatalf("renewed %v, want %v (revoked and non-oauth keys left out)", ids, want)
	}
}
