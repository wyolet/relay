//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/pkg/ids"
)

// A default account no key names is autofill's, so it never keeps its host, team or project from being deleted and goes with it; a named account, or a default one a key still names, keeps blocking.
func TestIntegration_KeylessDefaultAccountsGoWithTheirOwner(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	ctx := context.Background()
	system := meta.Owner{Kind: meta.OwnerSystem}
	mkHost := func(name string) string {
		h := &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: system}, Spec: host.Spec{BaseURL: "https://" + name + ".example.com"}}
		mustUpsert(t, st.stores.Host.Upsert(ctx, h), "host "+name)
		return h.Meta.ID
	}
	defaultAccount := func(owner meta.Owner, hostID string) string {
		id, err := st.stores.Account.EnsureDefault(ctx, owner, hostID)
		if err != nil || id == "" {
			t.Fatalf("EnsureDefault(%+v) = %q, %v", owner, id, err)
		}
		return id
	}
	gone := func(id string) bool {
		a, err := st.stores.Account.Get(ctx, id)
		if err != nil {
			t.Fatalf("get account: %v", err)
		}
		return a == nil
	}
	del := func(path string, want int) string {
		t.Helper()
		code, raw := st.adminDo(http.MethodDelete, path, "")
		if code != want {
			t.Fatalf("DELETE %s = %d, want %d: %s", path, code, want, raw)
		}
		return string(raw)
	}
	shared := mkHost("defaults-shared")

	// A host takes its keyless default account along.
	lone := mkHost("defaults-lone")
	hostDefault := defaultAccount(system, lone)
	del("/api/hosts/by-id/"+lone, http.StatusNoContent)
	if !gone(hostDefault) {
		t.Error("the host's keyless default account outlived the host")
	}

	// So does a team, and a project.
	teamA := st.mkTeam(t, "defaults-a")
	teamDefault := defaultAccount(meta.Owner{Kind: meta.OwnerTeam, ID: teamA}, shared)
	del("/api/teams/by-id/"+teamA, http.StatusNoContent)
	teamB := st.mkTeam(t, "defaults-b")
	projB := st.mkProject(t, "defaults-b-one", teamB)
	projectDefault := defaultAccount(meta.Owner{Kind: meta.OwnerProject, ID: projB}, shared)
	del("/api/projects/by-id/"+projB, http.StatusNoContent)
	if !gone(teamDefault) || !gone(projectDefault) {
		t.Errorf("keyless default accounts outlived their owners: team gone %v, project gone %v", gone(teamDefault), gone(projectDefault))
	}

	// A default account a key still names blocks, and so does a named one.
	teamC := st.mkTeam(t, "defaults-c")
	used := defaultAccount(meta.Owner{Kind: meta.OwnerTeam, ID: teamC}, shared)
	key := &hostkey.HostKey{Meta: meta.Metadata{ID: ids.New(), Name: "defaults-key", Owner: system},
		Spec: hostkey.Spec{HostID: shared, AccountID: used, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "DEFAULTS_UNSET_KEY"}}}
	mustUpsert(t, st.stores.HostKey.Upsert(ctx, key), "host key")
	if raw := del("/api/teams/by-id/"+teamC, http.StatusConflict); !strings.Contains(raw, `"account"`) {
		t.Errorf("409 for a team whose default account a key names does not name the account: %s", raw)
	}
	teamD := st.mkTeam(t, "defaults-d")
	named := &account.Account{Meta: meta.Metadata{ID: ids.New(), Name: "defaults-named", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: teamD}}, Spec: account.Spec{HostID: shared}}
	mustUpsert(t, st.stores.Account.Upsert(ctx, named), "named account")
	del("/api/teams/by-id/"+teamD, http.StatusConflict)
	if gone(used) || gone(named.Meta.ID) {
		t.Error("a refused delete removed an account")
	}
}
