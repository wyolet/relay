package manifest_test

import (
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/serviceaccount"
	"testing"
	"time"

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

// A service account takes its project as owner, a group its members by id, a key its principal by id; each exports back to its document.
func TestRoundTrip_Subjects(t *testing.T) {
	docs := parseDocs(t, subjectsYAML, 3)
	inProject := meta.Owner{Kind: meta.OwnerProject, ID: projectUUID}

	sa := translated(t, "ToServiceAccount", &serviceaccount.ServiceAccount{
		Meta: meta.Metadata{Name: "search-indexer", Owner: inProject, Annotations: map[string]string{"wyolet.com/runbook": "https://runbooks.example/indexer"}},
		Spec: serviceaccount.Spec{ProjectID: projectUUID},
	})(manifest.ToServiceAccount(*docs[0].ServiceAccount, subjectsResolver))
	wantSA := *docs[0].ServiceAccount
	wantSA.Metadata.Owner = manifest.WireOwner{Kind: meta.OwnerProject, Name: "ml-search"}
	exported(t, "FromServiceAccount", wantSA, manifest.FromServiceAccount(sa, subjectsRev))

	g := translated(t, "ToGroup", &group.Group{
		Meta: meta.Metadata{Name: "data-science", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: group.Spec{MemberIDs: []string{aliceUUID, bobUUID}},
	})(manifest.ToGroup(*docs[1].Group, subjectsResolver))
	exported(t, "FromGroup", *docs[1].Group, manifest.FromGroup(g, subjectsRev))

	expires := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	k := translated(t, "ToKey", &key.Key{
		Meta: meta.Metadata{Name: "search-indexer-prod", Owner: inProject},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: saUUID},
			KeyHash:   "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			Prefix:    "rk_a8b3f2",
			ExpiresAt: &expires,
		},
	})(manifest.ToKey(*docs[2].Key, subjectsResolver))
	// FromKey leaves the owner as its id; the export names it afterwards.
	wantKey := *docs[2].Key
	wantKey.Metadata.Owner.Name = projectUUID
	exported(t, "FromKey", wantKey, manifest.FromKey(k, subjectsRev))
}

// A user's key resolves its principal; the user owner is left for the write path to stamp.
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
	doc := parseOne(t, yaml)
	k := translated(t, "ToKey", &key.Key{
		Meta: meta.Metadata{Name: "alice-personal", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: aliceUUID}, KeyHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
	})(manifest.ToKey(*doc.Key, subjectsResolver))
	exported(t, "FromKey", *doc.Key, manifest.FromKey(k, subjectsRev))
}
