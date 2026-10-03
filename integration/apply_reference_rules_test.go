//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/pkg/ids"
)

// tenantPair is two teams with a project each: the caller administers the
// first, the second holds credentials the caller must never reach.
type tenantPair struct {
	st                *stack
	callerID          string
	ownProj, victProj string
	victimSA          string
}

func newTenantPair(t *testing.T) tenantPair {
	t.Helper()
	ctx := context.Background()
	st := newStackAuthz(t, "rbac")
	roles := seedBuiltinRoles(t, st)
	if err := st.cat.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	ownTeam := st.mkTeam(t, "own-team")
	victTeam := st.mkTeam(t, "victim-team")
	p := tenantPair{st: st}
	p.ownProj = st.mkProject(t, "own-proj", ownTeam)
	p.victProj = st.mkProject(t, "victim-proj", victTeam)
	p.victimSA = st.mkServiceAccount(t, "victim-sa", p.victProj)

	hst := &host.Host{
		Meta: meta.Metadata{ID: ids.New(), Name: "shared-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "http://upstream.invalid"},
	}
	mustUpsert(t, st.stores.Host.Upsert(ctx, hst), "host")
	tier := &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: "shared-host-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hst.Meta.ID}}}
	mustUpsert(t, st.stores.Policy.Upsert(ctx, tier), "tier")
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: ids.New(), Name: "victim-hk", Owner: meta.Owner{Kind: meta.OwnerProject, ID: p.victProj}},
		Spec: hostkey.Spec{HostID: hst.Meta.ID, PolicyID: tier.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "VICTIM_HK"}},
	}
	mustUpsert(t, st.stores.HostKey.Upsert(ctx, hk), "victim host key")
	vpol := &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: "victim-pol", Owner: meta.Owner{Kind: meta.OwnerProject, ID: p.victProj}},
		Spec: policy.Spec{HostKeyIDs: []string{hk.Meta.ID}}}
	mustUpsert(t, st.stores.Policy.Upsert(ctx, vpol), "victim policy")

	p.callerID = st.seedLogin(t, "mallory", "pw-mallory")
	st.bindRole(t, "own-admins", roles["team-admin"].Meta.ID, p.callerID, "team", ownTeam)
	if err := st.cat.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return p
}

// A bundle runs the reference rules a POST runs: another tenant's host key,
// service account, or policy is out of reach whichever write path names it.
func TestIntegration_ApplyRefusesForeignReferences(t *testing.T) {
	p := newTenantPair(t)
	caller := p.st.login(t, "mallory", "pw-mallory")
	header := "apiVersion: relay.wyolet.dev/v1alpha2\n"
	cases := map[string]string{
		"policy spending another project's host key": header + `kind: Policy
metadata: {name: stolen-keys, owner: {kind: project, name: own-proj}}
spec: {hostKeys: [victim-hk]}
`,
		"key acting as another project's service account": header + `kind: Key
metadata: {name: stolen-sa, owner: {kind: project, name: own-proj}}
spec: {principal: {kind: serviceaccount, name: victim-sa}, keyHash: "` + sha256Hex("stolen") + `"}
`,
		"service account bound to another project's policy": header + `kind: ServiceAccount
metadata: {name: stolen-pol}
spec: {project: own-proj, policy: victim-pol}
`,
		"non-admin host key reading the relay's environment": header + `kind: HostKey
metadata: {name: env-probe, owner: {kind: project, name: own-proj}}
spec: {hostId: shared-host, policyId: shared-host-tier, valueFrom: {kind: env, env: RELAY_MASTER_KEY}}
`,
	}
	for name, bundle := range cases {
		t.Run(name, func(t *testing.T) {
			code, raw := caller.doAs(http.MethodPost, "/api/apply", bundle, "application/yaml")
			if code == http.StatusOK {
				t.Fatalf("apply = 200, want refused: %s", raw)
			}
			if strings.Contains(string(raw), `"applied":true`) {
				t.Fatalf("apply wrote rows: %s", raw)
			}
		})
	}
	ctx := context.Background()
	keys, _ := p.st.stores.Key.List(ctx)
	for _, k := range keys {
		if k.Meta.Name == "stolen-sa" {
			t.Fatal("key acting as another project's service account was written")
		}
	}
	pols, _ := p.st.stores.Policy.List(ctx)
	for _, pol := range pols {
		if pol.Meta.Name == "stolen-keys" {
			t.Fatal("policy spending another project's host key was written")
		}
	}
}
