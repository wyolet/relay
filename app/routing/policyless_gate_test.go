package routing

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/settings"
)

// openPolicyless answers the inference section with policy-less traffic
// switched on, which is the only setting the gate reads.
type openPolicyless struct{}

func (openPolicyless) Setting(section string) (any, bool) {
	if section != settings.SectionInference {
		return nil, false
	}
	return &settings.Inference{AllowMissingPolicy: true}, true
}

// D82: the operator setting opens policy-less traffic only under
// single-user authorization. Under rbac the grants a credential carries are
// the whole access model, so a key whose policy does not resolve is refused
// with the same missing-policy error the setting-off path answers.
func TestResolve_PolicylessHonouredOnlyUnderSingleAuthorization(t *testing.T) {
	f := newTwoHostParts()
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{f.bindingA},
	)

	single := &Resolver{cfg: openPolicyless{}}
	if !single.PolicylessTrafficAllowed() {
		t.Fatal("the setting is on and authorization is single, but the gate is closed")
	}
	if _, err := single.Resolve(Request{ModelName: "m1", Snapshot: snap}); err != nil {
		t.Fatalf("Resolve under single authorization: %v", err)
	}

	rbac := &Resolver{cfg: openPolicyless{}, requirePolicy: true}
	if rbac.PolicylessTrafficAllowed() {
		t.Error("the listing would advertise models to a caller the flow refuses")
	}
	if _, err := rbac.Resolve(Request{ModelName: "m1", Snapshot: snap}); !errors.Is(err, ErrPolicyless) {
		t.Fatalf("err = %v, want ErrPolicyless — rbac served a key with no policy", err)
	}
}
