package catalogview

import (
	"context"
	"errors"
	"testing"
)

// A pool lists the keys of its accounts, then its own, in pool order; an account lists the keys naming it, by name. Rows the caller may not see drop out, except a share's, which show read-only.
func TestPoolAndAccountKeys(t *testing.T) {
	ctx := context.Background()
	zKey, bKey := inAccount(keyOf("z-key", "h", alice), "org"), inAccount(keyOf("b-key", "h", alice), "org")
	bobsKey := inAccount(keyOf("bobs-key", "h", bob), "bobs")
	bobsKey.Resolved = "sk-ant-api03-ABCDEFGHIJKLMNOP1234"
	svc := newService(
		zKey, bKey, bobsKey, inAccount(keyOf("hidden-key", "h", bob), "org"), keyOf("loose-key", "h", alice),
		accountOf("org", "h", alice, ""), accountOf("bobs", "h", bob, ""), accountOf("bobs-for-alice", "h", alice, "bobs"),
		poolOf("mine", "h", alice, "z-key", "hidden-key", "deleted-key", "b-key", "z-key"),
		drawingOn(poolOf("with-accounts", "h", alice, "loose-key", "b-key"), "bobs-for-alice", "deleted", "org"),
		poolOf("theirs", "h", bob),
	)
	svc.Visible = asAlice
	shared := HostKeyView{ID: "key:bobs-key", Name: "bobs-key", Enabled: true, ReadOnly: true, Prefix: "sk-ant-...1234"}

	viewed(t, "pool order", UpstreamRef{ID: "pool:mine", Name: "mine", HostID: "host:h"},
		[]HostKeyView{keyView("z-key"), keyView("b-key")})(svc.PoolKeys(ctx, "mine"))
	viewed(t, "accounts first", UpstreamRef{ID: "pool:with-accounts", Name: "with-accounts", HostID: "host:h"},
		[]HostKeyView{shared, keyView("b-key"), keyView("z-key"), keyView("loose-key")})(svc.PoolKeys(ctx, "pool:with-accounts"))
	viewed(t, "account by id", UpstreamRef{ID: "account:org", Name: "org", HostID: "host:h"},
		[]HostKeyView{keyView("b-key"), keyView("z-key")})(svc.AccountKeys(ctx, "account:org"))
	viewed(t, "shared account", UpstreamRef{ID: "account:bobs-for-alice", Name: "bobs-for-alice", HostID: "host:h"},
		[]HostKeyView{shared})(svc.AccountKeys(ctx, "bobs-for-alice"))

	for name, err := range map[string]error{
		"pool of another owner":    third(svc.PoolKeys(ctx, "theirs")),
		"account of another owner": third(svc.AccountKeys(ctx, "bobs")),
		"no pool store":            third(newService().PoolKeys(ctx, "mine")),
		"no account store":         third(newService().AccountKeys(ctx, "org")),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}

	// Without an account store a pool still lists the keys it names.
	svc.Accounts = nil
	viewed(t, "no account store", UpstreamRef{ID: "pool:with-accounts", Name: "with-accounts", HostID: "host:h"},
		[]HostKeyView{keyView("loose-key"), keyView("b-key")})(svc.PoolKeys(ctx, "with-accounts"))
}
