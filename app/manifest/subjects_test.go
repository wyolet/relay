package manifest_test

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/pkg/meta"
)

const subjectsYAML = `
apiVersion: relay.wyolet.dev/v1alpha3
kind: ServiceAccount
metadata:
  name: search-indexer
  annotations: {wyolet.com/runbook: "https://runbooks.example/indexer"}
spec:
  project: ml-search
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Group
metadata:
  name: data-science
  owner: {kind: system}
spec:
  members: [alice, bob]
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Key
metadata:
  name: search-indexer-prod
  owner: {kind: project, name: ml-search}
spec:
  principal: {kind: serviceaccount, name: search-indexer}
  keyHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
  prefix: rk_a8b3f2
  expiresAt: "2027-01-01T00:00:00Z"
`

const (
	projectUUID = "0195f8a0-0000-7000-8000-000000000011"
	policyUUID  = "0195f8a0-0000-7000-8000-000000000012"
	saUUID      = "0195f8a0-0000-7000-8000-000000000013"
	aliceUUID   = "0195f8a0-0000-7000-8000-000000000014"
	bobUUID     = "0195f8a0-0000-7000-8000-000000000015"
)

var subjectsResolver = manifest.MapResolver{
	Projects:        map[string]string{"ml-search": projectUUID},
	Policies:        map[string]string{"ml-search-default": policyUUID},
	ServiceAccounts: map[string]string{"search-indexer": saUUID},
	Users:           map[string]string{"alice": aliceUUID, "bob": bobUUID},
}

var subjectsRev = manifest.MapReverseResolver{
	Projects:        map[string]string{projectUUID: "ml-search"},
	Policies:        map[string]string{policyUUID: "ml-search-default"},
	ServiceAccounts: map[string]string{saUUID: "search-indexer"},
	Users:           map[string]string{aliceUUID: "alice", bobUUID: "bob"},
}

func TestRoundTrip_Subjects(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(subjectsYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(docs) != 3 || docs[0].ServiceAccount == nil || docs[1].Group == nil || docs[2].Key == nil {
		t.Fatalf("want ServiceAccount + Group + Key docs, got %d: %v", len(docs), docs)
	}

	sa, err := manifest.ToServiceAccount(*docs[0].ServiceAccount, subjectsResolver)
	if err != nil {
		t.Fatalf("ToServiceAccount: %v", err)
	}
	if sa.Spec.ProjectID != projectUUID {
		t.Errorf("service account project = %q", sa.Spec.ProjectID)
	}
	if sa.Meta.Owner.ID != projectUUID {
		t.Errorf("service account owner = %+v, want the project", sa.Meta.Owner)
	}
	if back := manifest.FromServiceAccount(sa, subjectsRev); back.Spec.Project != "ml-search" {
		t.Errorf("FromServiceAccount = %+v", back.Spec)
	}

	g, err := manifest.ToGroup(*docs[1].Group, subjectsResolver)
	if err != nil {
		t.Fatalf("ToGroup: %v", err)
	}
	if len(g.Spec.MemberIDs) != 2 || g.Spec.MemberIDs[0] != aliceUUID || g.Spec.MemberIDs[1] != bobUUID {
		t.Errorf("group members = %v", g.Spec.MemberIDs)
	}
	if back := manifest.FromGroup(g, subjectsRev); strings.Join(back.Spec.Members, ",") != "alice,bob" {
		t.Errorf("FromGroup = %v", back.Spec.Members)
	}

	k, err := manifest.ToKey(*docs[2].Key, subjectsResolver)
	if err != nil {
		t.Fatalf("ToKey: %v", err)
	}
	if k.Spec.Principal.Kind != key.PrincipalServiceAccount || k.Spec.Principal.ID != saUUID {
		t.Errorf("key principal = %+v", k.Spec.Principal)
	}
	if k.Meta.Owner.ID != projectUUID {
		t.Errorf("key owner = %+v, want the project id", k.Meta.Owner)
	}
	if k.Spec.ExpiresAt == nil || k.Spec.ExpiresAt.Year() != 2027 {
		t.Errorf("key expiresAt = %v", k.Spec.ExpiresAt)
	}
	back := manifest.FromKey(k, subjectsRev)
	if back.Spec.Principal.Kind != "serviceaccount" || back.Spec.Principal.Name != "search-indexer" {
		t.Errorf("FromKey principal = %+v", back.Spec.Principal)
	}
	if back.Spec.ExpiresAt == nil || *back.Spec.ExpiresAt != "2027-01-01T00:00:00Z" {
		t.Errorf("FromKey expiresAt = %v", back.Spec.ExpiresAt)
	}
}

func TestParse_UserPrincipalKey(t *testing.T) {
	const yaml = `
apiVersion: relay.wyolet.dev/v1alpha3
kind: Key
metadata:
  name: alice-personal
  owner: {kind: user}
spec:
  principal: {kind: user, name: alice}
  keyHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
`
	docs, err := manifest.Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	k, err := manifest.ToKey(*docs[0].Key, subjectsResolver)
	if err != nil {
		t.Fatalf("ToKey: %v", err)
	}
	if want := (key.Principal{Kind: key.PrincipalUser, ID: aliceUUID}); k.Spec.Principal != want || k.Meta.Owner != (meta.Owner{Kind: meta.OwnerUser}) {
		t.Errorf("key principal %+v owner %+v, want alice and a user owner left for the write path to stamp", k.Spec.Principal, k.Meta.Owner)
	}
}
