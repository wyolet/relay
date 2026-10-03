package apply

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

func entryFor(t *testing.T, b *builder, kind, name string) Entry {
	t.Helper()
	for _, e := range b.entries {
		if e.Kind == kind && e.Name == name {
			return e
		}
	}
	t.Fatalf("no %s %q in the plan", kind, name)
	return Entry{}
}

// storedHostKeyRows is a host, its tier policy, and a host key whose secret
// lives encrypted in the database.
func storedHostKeyRows(kind hostkey.ValueKind) (*Rows, *hostkey.HostKey) {
	h := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-api", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	tier := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: h.Meta.ID}}}
	hk := &hostkey.HostKey{
		Meta:     meta.Metadata{ID: meta.NewID(), Name: "acme-key", DisplayName: "Acme", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec:     hostkey.Spec{HostID: h.Meta.ID, PolicyID: tier.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: kind}},
		Resolved: "sk-stored-secret",
	}
	if kind == hostkey.ValueKindOAuth {
		hk.Spec.ValueFrom.Provider = "acme"
	}
	return &Rows{Hosts: []*host.Host{h}, Policies: []*policy.Policy{tier}, HostKeys: []*hostkey.HostKey{hk}}, hk
}

// An exported stored or oauth host key carries no value. Applying it back
// with an edit must keep the stored secret rather than fail as if the
// secret were missing.
func TestApplyUpdatesAStoredHostKeyWithoutItsValue(t *testing.T) {
	for _, kind := range []hostkey.ValueKind{hostkey.ValueKindStored, hostkey.ValueKindOAuth} {
		t.Run(string(kind), func(t *testing.T) {
			rows, hk := storedHostKeyRows(kind)
			doc := &manifest.HostKeyDTO{APIVersion: manifest.APIVersion, Kind: "HostKey"}
			doc.Metadata.Name = hk.Meta.Name
			doc.Metadata.DisplayName = "Acme (renamed)"
			doc.Spec = manifest.HostKeySpec{
				HostID: "acme-api", PolicyID: "acme-tier",
				ValueFrom: manifest.HostKeyValueFrom{Kind: string(kind), Provider: hk.Spec.ValueFrom.Provider},
			}
			b := &builder{opts: Options{Stores: &Stores{}}, rows: rows, idx: newIndex(rows)}
			if err := b.run(context.Background(), []manifest.Document{{HostKey: doc}}); err != nil {
				t.Fatalf("plan: %v", err)
			}
			if e := entryFor(t, b, "HostKey", hk.Meta.Name); e.Action != ActionUpdate {
				t.Fatalf("action = %s, want update", e.Action)
			}
		})
	}
}

// keepHostKeySecret is what the update writes: no value means the stored
// secret stays; an explicit value replaces it.
func TestKeepHostKeySecret(t *testing.T) {
	_, prev := storedHostKeyRows(hostkey.ValueKindStored)
	next := &hostkey.HostKey{Spec: prev.Spec}
	keepHostKeySecret(prev, next)
	if next.Resolved != prev.Resolved || next.Spec.Value != "" {
		t.Fatalf("update without a value: resolved=%q value=%q, want the stored secret kept", next.Resolved, next.Spec.Value)
	}

	rotated := &hostkey.HostKey{Spec: prev.Spec}
	rotated.Spec.Value = "sk-new"
	keepHostKeySecret(prev, rotated)
	if rotated.Resolved != "" || rotated.Spec.Value != "sk-new" {
		t.Fatalf("update with a value: resolved=%q value=%q, want the new value", rotated.Resolved, rotated.Spec.Value)
	}

	// A row switching from an env ref has no stored secret to keep.
	envPrev := &hostkey.HostKey{Spec: hostkey.Spec{ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "X"}}, Resolved: "from-env"}
	switched := &hostkey.HostKey{Spec: prev.Spec}
	keepHostKeySecret(envPrev, switched)
	if switched.Resolved != "" {
		t.Fatalf("env → stored carried the env value over as the stored secret")
	}
}

// rotatedKeyRows is a service account key mid-rotation: the old hash still
// authenticates until graceUntil.
func rotatedKeyRows() (*Rows, *key.Key) {
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	proj := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-search"}, Spec: project.Spec{TeamID: tm.Meta.ID}}
	proj.StampOwner()
	sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer"}, Spec: serviceaccount.Spec{ProjectID: proj.Meta.ID}}
	sa.StampOwner()
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer-key", Owner: meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}},
		Spec: key.Spec{
			Principal:       key.Principal{Kind: key.PrincipalServiceAccount, ID: sa.Meta.ID},
			KeyHash:         strings.Repeat("a", 64),
			PreviousKeyHash: strings.Repeat("b", 64),
			GraceUntil:      &until,
		},
	}
	return &Rows{
		Teams: []*team.Team{tm}, Projects: []*project.Project{proj},
		ServiceAccounts: []*serviceaccount.ServiceAccount{sa}, Keys: []*key.Key{k},
	}, k
}

func keyDoc(k *key.Key) *manifest.KeyDTO {
	d := &manifest.KeyDTO{APIVersion: manifest.APIVersion, Kind: "Key"}
	d.Metadata.Name = k.Meta.Name
	d.Metadata.Owner = manifest.WireOwner{Kind: meta.OwnerProject, Name: "ml-search"}
	d.Spec.Principal = manifest.PrincipalDTO{Kind: "serviceaccount", Name: "indexer"}
	d.Spec.KeyHash = k.Spec.KeyHash
	return d
}

// The rotation grace window is server state a manifest cannot author:
// re-applying an exported key is not a change, and an edit keeps it.
func TestApplyLeavesKeyRotationStateAlone(t *testing.T) {
	rows, k := rotatedKeyRows()
	b := &builder{opts: Options{Stores: &Stores{}}, rows: rows, idx: newIndex(rows)}
	if err := b.run(context.Background(), []manifest.Document{{Key: keyDoc(k)}}); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if e := entryFor(t, b, "Key", k.Meta.Name); e.Action != ActionUnchanged {
		t.Fatalf("action = %s (fields %v), want unchanged", e.Action, e.ChangedFields)
	}

	next := &key.Key{Spec: key.Spec{KeyHash: k.Spec.KeyHash}}
	keepKeyServerFields(k, next)
	if next.Spec.PreviousKeyHash != k.Spec.PreviousKeyHash || next.Spec.GraceUntil != k.Spec.GraceUntil {
		t.Fatalf("update dropped the rotation state: %+v", next.Spec)
	}
}
