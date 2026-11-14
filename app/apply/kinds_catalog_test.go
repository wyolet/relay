package apply

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/meta"
)

// storedHostKeyRows is a host, its provider account "acme-org" and a host key
// in it whose secret lives encrypted in the database.
func storedHostKeyRows(kind hostkey.ValueKind) (*Rows, *hostkey.HostKey) {
	h := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-api", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	org := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-org", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: h.Meta.ID}}
	hk := &hostkey.HostKey{
		Meta:     meta.Metadata{ID: meta.NewID(), Name: "acme-key", DisplayName: "Acme", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec:     hostkey.Spec{HostID: h.Meta.ID, AccountID: org.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: kind}},
		Resolved: "sk-stored-secret",
	}
	if kind == hostkey.ValueKindOAuth {
		hk.Spec.ValueFrom.Provider = "acme"
	}
	return &Rows{Hosts: []*host.Host{h}, Accounts: []*account.Account{org}, HostKeys: []*hostkey.HostKey{hk}}, hk
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
				HostID:    "acme-api",
				Account:   "acme-org",
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

// apply goes through the row's Validate, so a manifest cannot point a key at relay's own configuration either.
func TestApplyRefusesAHostKeyOnAReservedEnvRef(t *testing.T) {
	for env, refused := range map[string]bool{"RELAY_MASTER_KEY": true, "WYOLET_OIDC_CLIENT_SECRET": true, "OPENAI_API_KEY": false} {
		t.Run(env, func(t *testing.T) {
			rows, _ := storedHostKeyRows(hostkey.ValueKindStored)
			rows.HostKeys = nil
			doc := &manifest.HostKeyDTO{APIVersion: manifest.APIVersion, Kind: "HostKey"}
			doc.Metadata.Name = "env-key"
			doc.Spec = manifest.HostKeySpec{
				HostID:    "acme-api",
				Account:   "acme-org",
				ValueFrom: manifest.HostKeyValueFrom{Kind: string(hostkey.ValueKindEnv), Env: env},
			}
			b := &builder{opts: Options{Stores: &Stores{}}, rows: rows, idx: newIndex(rows)}
			err := b.run(context.Background(), []manifest.Document{{HostKey: doc}})
			if !refused {
				if err != nil {
					t.Fatalf("plan: %v", err)
				}
				return
			}
			wantRefused(t, err, "HostKey", "env-key", hostkey.ErrReservedEnv)
		})
	}
}
