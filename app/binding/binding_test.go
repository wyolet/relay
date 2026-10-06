package binding

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func valid(owner meta.Owner) *Binding {
	return &Binding{
		Meta: meta.Metadata{Name: "m-on-h", Owner: owner},
		Spec: Spec{ModelID: meta.NewID(), HostID: meta.NewID()},
	}
}

func TestValidate_DeploymentOwnersAccepted(t *testing.T) {
	for _, o := range []meta.Owner{{}, {Kind: meta.OwnerSystem}} {
		if err := valid(o).Validate(); err != nil {
			t.Errorf("owner %+v: %v", o, err)
		}
	}
}

// A binding decides which host serves a model's traffic, so it is shared
// catalog data: no user, team or project owns one.
func TestValidate_TenantOwnersRefused(t *testing.T) {
	for _, o := range []meta.Owner{
		{Kind: meta.OwnerUser}, {Kind: meta.OwnerUser, ID: "u-1"},
		{Kind: meta.OwnerTeam, ID: "t-1"}, {Kind: meta.OwnerProject, ID: "p-1"},
	} {
		err := valid(o).Validate()
		if err == nil || !strings.Contains(err.Error(), "shared catalog data") {
			t.Errorf("owner %+v: err = %v, want a shared catalog data refusal", o, err)
		}
	}
}
